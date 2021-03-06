/*
Copyright 2015 Gravitational, Inc.

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

// Package trace implements utility functions for capturing logs
package trace

import (
	"strings"

	log "github.com/Sirupsen/logrus"

	"runtime"
)

const (
	// FileField is a field with code file added to structured traces
	FileField = "file"
	// FunctionField is a field with function name
	FunctionField = "func"
	// LevelField returns logging level as set by logrus
	LevelField = "level"
	// Component is a field that represents component - e.g. service or
	// function
	Component = "component"
)

// TextFormatter is logrus-compatible formatter and adds
// file and line details to every logged entry.
type TextFormatter struct {
	log.TextFormatter
}

// Format implements logrus.Formatter interface and adds file and line
func (tf *TextFormatter) Format(e *log.Entry) ([]byte, error) {
	if frameNo := findFrame(); frameNo != -1 {
		t := newTrace(runtime.Caller(frameNo - 1))
		e.Data[FileField] = t.String()
	}
	return (&tf.TextFormatter).Format(e)
}

// JSONFormatter implements logrus.Formatter interface and adds file and line
// properties to JSON entries
type JSONFormatter struct {
	log.JSONFormatter
}

// Format implements logrus.Formatter interface
func (j *JSONFormatter) Format(e *log.Entry) ([]byte, error) {
	if frameNo := findFrame(); frameNo != -1 {
		t := newTrace(runtime.Caller(frameNo - 1))
		e.Data[FileField] = t.String()
		e.Data[FunctionField] = t.Func
	}
	return (&j.JSONFormatter).Format(e)
}

func findFrame() int {
	for i := 3; i < 10; i++ {
		_, file, _, ok := runtime.Caller(i)
		if !ok {
			return -1
		}
		if !strings.Contains(file, "Sirupsen/logrus") {
			return i
		}
	}
	return -1
}
