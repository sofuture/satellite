/*
Copyright 2016 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package agent

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gravitational/satellite/agent/cache"
	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/trace"

	log "github.com/Sirupsen/logrus"
	serf "github.com/hashicorp/serf/client"
	"github.com/jonboulle/clockwork"
	"golang.org/x/net/context"
)

// Agent is the interface to interact with the monitoring agent.
type Agent interface {
	// Start starts agent's background jobs.
	Start() error
	// Close stops background activity and releases resources.
	Close() error
	// Join makes an attempt to join a cluster specified by the list of peers.
	Join(peers []string) error
	// LocalStatus reports the health status of the local agent node.
	LocalStatus() *pb.NodeStatus

	health.CheckerRepository
}

type Config struct {
	// Name of the agent unique within the cluster.
	// Names are used as a unique id within a serf cluster, so
	// it is important to avoid clashes.
	//
	// Name must match the name of the local serf agent so that the agent
	// can match itself to a serf member.
	Name string

	// RPCAddrs is a list of addresses agent binds to for RPC traffic.
	//
	// Usually, at least two address are used for operation.
	// Localhost is a convenience for local communication.  Cluster-visible
	// IP is required for proper inter-communication between agents.
	RPCAddrs []string

	// RPC address of local serf node.
	SerfRPCAddr string

	// Peers lists the nodes that are part of the initial serf cluster configuration.
	// This is not a final cluster configuration and new nodes or node updates
	// are still possible.
	Peers []string

	// Set of tags for the agent.
	// Tags is a trivial means for adding extra semantic information to an agent.
	Tags map[string]string

	// Cache is a short-lived storage used by the agent to persist latest health stats.
	Cache cache.Cache
}

// New creates an instance of an agent based on configuration options given in config.
func New(config *Config) (Agent, error) {
	clientConfig := &serf.Config{
		Addr: config.SerfRPCAddr,
	}
	client, err := serf.ClientFromConfig(clientConfig)
	if err != nil {
		return nil, trace.Wrap(err, "failed to connect to serf")
	}
	if config.Tags == nil {
		config.Tags = make(map[string]string)
	}
	err = client.UpdateTags(config.Tags, nil)
	if err != nil {
		return nil, trace.Wrap(err, "failed to update serf agent tags")
	}
	var listeners []net.Listener
	defer func() {
		if err != nil {
			for _, listener := range listeners {
				listener.Close()
			}
		}
	}()
	for _, addr := range config.RPCAddrs {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		listeners = append(listeners, listener)
	}
	agent := &agent{
		serfClient:  client,
		name:        config.Name,
		cache:       config.Cache,
		dialRPC:     defaultDialRPC,
		clock:       clockwork.NewRealClock(),
		localStatus: emptyNodeStatus(config.Name),
	}
	agent.rpc = newRPCServer(agent, listeners)
	return agent, nil
}

type agent struct {
	health.Checkers

	// serfClient provides access to the serf agent.
	serfClient serfClient

	// Name of this agent.  Must be the same as the serf agent's name
	// running on the same node.
	name string

	// RPC server used by agent for client communication as well as
	// status sync with other agents.
	rpc RPCServer

	// cache persists node status history.
	cache cache.Cache

	// dialRPC is a factory function to create clients to other agents.
	// If future, agent address discovery will happen through serf.
	dialRPC dialRPC

	// done is a channel used for cleanup.
	done chan struct{}

	// clock abstracts away access to the time package to allow
	// testing.
	clock clockwork.Clock

	mu sync.Mutex
	// localStatus is the last obtained local node status.
	localStatus *pb.NodeStatus
}

// Start starts the agent's background tasks.
func (r *agent) Start() error {
	r.done = make(chan struct{})

	go r.statusUpdateLoop()
	return nil
}

// Join attempts to join a serf cluster identified by peers.
func (r *agent) Join(peers []string) error {
	noReplay := false
	numJoined, err := r.serfClient.Join(peers, noReplay)
	if err != nil {
		return trace.Wrap(err)
	}
	log.Infof("joined %d nodes", numJoined)
	return nil
}

// Close stops all background activity and releases the agent's resources.
func (r *agent) Close() (err error) {
	r.rpc.Stop()
	close(r.done)
	err = r.serfClient.Close()
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// LocalStatus reports the status of the local agent node.
func (r *agent) LocalStatus() *pb.NodeStatus {
	return r.recentLocalStatus()
}

type dialRPC func(*serf.Member) (*client, error)

// runChecks executes the monitoring tests configured for this agent.
func (r *agent) runChecks(ctx context.Context) *pb.NodeStatus {
	var reporter health.Probes
	// TODO: run tests in parallel
	for _, c := range r.Checkers {
		log.Infof("running checker %s", c.Name())
		c.Check(&reporter)
	}
	status := &pb.NodeStatus{
		Name:   r.name,
		Status: reporter.Status(),
		Probes: reporter.GetProbes(),
	}
	return status
}

// statusUpdateTimeout is the amount of time to wait between status update collections.
const statusUpdateTimeout = 30 * time.Second

// statusQueryWaitTimeout is the amount of time to wait for status query reply.
const statusQueryWaitTimeout = 10 * time.Second

// statusUpdateLoop is a long running background process that periodically
// updates the health status of the cluster by querying status of other active
// cluster members.
func (r *agent) statusUpdateLoop() {
	for {
		select {
		case <-r.clock.After(statusUpdateTimeout):
			ctx, cancel := context.WithTimeout(context.Background(), statusQueryWaitTimeout)
			go func() {
				defer cancel() // close context if collection finishes before the deadline
				status, err := r.collectStatus(ctx)
				if err != nil {
					log.Infof("error collecting system status: %v", err)
					return
				}
				if err = r.cache.UpdateStatus(status); err != nil {
					log.Infof("error updating system status in cache: %v", err)
				}
			}()
			select {
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					log.Infof("timed out collecting system status")
				}
			case <-r.done:
				cancel()
				return
			}
		case <-r.done:
			return
		}
	}
}

// collectStatus obtains the cluster status by querying statuses of
// known cluster members.
func (r *agent) collectStatus(ctx context.Context) (systemStatus *pb.SystemStatus, err error) {
	systemStatus = &pb.SystemStatus{Status: pb.SystemStatus_Unknown, Timestamp: pb.NewTimestamp()}

	members, err := r.serfClient.Members()
	if err != nil {
		return nil, trace.Wrap(err, "failed to query serf members")
	}
	log.Infof("started collecting statuses from %d members: %v", len(members), members)

	statuses := make(chan *statusResponse, len(members))
	var wg sync.WaitGroup

	wg.Add(len(members))
	for _, member := range members {
		if r.name == member.Name {
			go r.getLocalStatus(ctx, member, statuses, &wg)
		} else {
			go r.getStatusFrom(ctx, member, statuses, &wg)
		}
	}
	wg.Wait()
	close(statuses)

	for status := range statuses {
		log.Infof("retrieved status from %v: %v", status.member, status.NodeStatus)
		nodeStatus := status.NodeStatus
		if status.err != nil {
			log.Infof("failed to query node %s(%v) status: %v", status.member.Name, status.member.Addr, status.err)
			nodeStatus = unknownNodeStatus(&status.member)
		}
		systemStatus.Nodes = append(systemStatus.Nodes, nodeStatus)
	}
	setSystemStatus(systemStatus)

	return systemStatus, nil
}

// collectLocalStatus executes monitoring tests on the local node.
func (r *agent) collectLocalStatus(ctx context.Context, local *serf.Member) (status *pb.NodeStatus, err error) {
	status = r.runChecks(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	status.MemberStatus = statusFromMember(local)
	r.mu.Lock()
	r.localStatus = status
	r.mu.Unlock()

	return status, nil
}

// getLocalStatus obtains local node status in background.
func (r *agent) getLocalStatus(ctx context.Context, local serf.Member, respc chan<- *statusResponse, wg *sync.WaitGroup) {
	defer wg.Done()
	status, err := r.collectLocalStatus(ctx, &local)
	resp := &statusResponse{member: local}
	if err != nil {
		resp.err = trace.Wrap(err)
	} else {
		resp.NodeStatus = status
	}
	select {
	case respc <- resp:
	case <-r.done:
	}
}

// getStatusFrom obtains node status from the node identified by member in background.
func (r *agent) getStatusFrom(ctx context.Context, member serf.Member, respc chan<- *statusResponse, wg *sync.WaitGroup) {
	defer wg.Done()
	client, err := r.dialRPC(&member)
	resp := &statusResponse{member: member}
	if err != nil {
		resp.err = trace.Wrap(err)
	} else {
		defer client.Close()
		var status *pb.NodeStatus
		status, err = client.LocalStatus(ctx)
		if err != nil {
			resp.err = trace.Wrap(err)
		} else {
			resp.NodeStatus = status
		}
	}
	select {
	case respc <- resp:
	case <-r.done:
	}
}

// statusResponse describes a status response from a background process that obtains
// health status on the specified serf node.
type statusResponse struct {
	*pb.NodeStatus
	member serf.Member
	err    error
}

// recentStatus returns the last known cluster status.
func (r *agent) recentStatus() (*pb.SystemStatus, error) {
	return r.cache.RecentStatus()
}

// recentLocalStatus returns the last known local node status.
func (r *agent) recentLocalStatus() *pb.NodeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.localStatus
}

func toMemberStatus(status string) pb.MemberStatus_Type {
	switch MemberStatus(status) {
	case MemberAlive:
		return pb.MemberStatus_Alive
	case MemberLeaving:
		return pb.MemberStatus_Leaving
	case MemberLeft:
		return pb.MemberStatus_Left
	case MemberFailed:
		return pb.MemberStatus_Failed
	}
	return pb.MemberStatus_None
}

// unknownNodeStatus creates an `unknown` node status for a node specified with member.
func unknownNodeStatus(member *serf.Member) *pb.NodeStatus {
	return &pb.NodeStatus{
		Name:         member.Name,
		Status:       pb.NodeStatus_Unknown,
		MemberStatus: statusFromMember(member),
	}
}

// emptyNodeStatus creates an empty node status.
func emptyNodeStatus(name string) *pb.NodeStatus {
	return &pb.NodeStatus{
		Name:         name,
		Status:       pb.NodeStatus_Unknown,
		MemberStatus: &pb.MemberStatus{Name: name},
	}
}

// emptySystemStatus creates an empty system status.
func emptySystemStatus() *pb.SystemStatus {
	return &pb.SystemStatus{
		Status: pb.SystemStatus_Unknown,
	}
}

// statusFromMember returns new member status value for the specified serf member.
func statusFromMember(member *serf.Member) *pb.MemberStatus {
	return &pb.MemberStatus{
		Name:   member.Name,
		Status: toMemberStatus(member.Status),
		Tags:   member.Tags,
		Addr:   fmt.Sprintf("%s:%d", member.Addr.String(), member.Port),
	}
}
