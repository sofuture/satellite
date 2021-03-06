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
package monitoring

import (
	"fmt"

	"github.com/gravitational/satellite/agent/health"
	"github.com/gravitational/trace"
)

// KubeAPIServerHealth creates a checker for the kubernetes API server
func KubeAPIServerHealth(kubeAddr string) health.Checker {
	return NewHTTPHealthzChecker("kube-apiserver", fmt.Sprintf("%v/healthz", kubeAddr), kubeHealthz)
}

// KubeletHealth creates a checker for the kubernetes kubelet component
func KubeletHealth(addr string) health.Checker {
	return NewHTTPHealthzChecker("kubelet", fmt.Sprintf("%v/healthz", addr), kubeHealthz)
}

// ComponentStatusHealth creates a checker of the kubernetes component statuses
func ComponentStatusHealth(kubeAddr string) health.Checker {
	return NewComponentStatusChecker(kubeAddr)
}

// EtcdHealth creates a checker that checks health of etcd
func EtcdHealth(config *ETCDConfig) (health.Checker, error) {
	const name = "etcd-healthz"

	transport, err := config.newHTTPTransport()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	createChecker := func(addr string) (health.Checker, error) {
		endpoint := fmt.Sprintf("%v/health", addr)
		return NewHTTPHealthzCheckerWithTransport(name, endpoint, transport, etcdChecker), nil
	}
	var checkers []health.Checker
	for _, endpoint := range config.Endpoints {
		checker, err := createChecker(endpoint)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		checkers = append(checkers, checker)
	}
	return &compositeChecker{name, checkers}, nil
}

// DockerHealth creates a checker that checks health of the docker daemon under
// the specified socketPath
func DockerHealth(socketPath string) health.Checker {
	return NewUnixSocketHealthzChecker("docker", "http://docker/version", socketPath,
		dockerChecker)
}

// SystemdHealth creates a checker that reports the status of systemd units
func SystemdHealth() health.Checker {
	return NewSystemdChecker()
}

// IntraPodCommunication creates a checker that runs a network test in the cluster
// by scheduling pods and verifying the communication
func IntraPodCommunication(kubeAddr, nettestImage string) health.Checker {
	return NewIntraPodChecker(kubeAddr, nettestImage)
}
