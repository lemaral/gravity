/*
Copyright 2018 Gravitational, Inc.

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

package install

import (
	"io/ioutil"
	"os"

	"github.com/gravitational/gravity/lib/defaults"

	log "github.com/sirupsen/logrus"
	"github.com/gravitational/trace"
)

// InitLogging initalizes logging for local installer
func InitLogging(logFile string) error {
	hook, err := NewLogHook(logFile)
	if err != nil {
		return trace.Wrap(err, "failed to create log file %v", logFile)
	}
	log.StandardLogger().Hooks.Add(hook)
	log.SetLevel(log.DebugLevel)
	log.SetOutput(ioutil.Discard)
	return nil
}

// NewLogHook returns new logging hook
func NewLogHook(path string) (*Hook, error) {
	// Truncate the contents of the log file
	f, err := os.Create(path)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	f.Close()
	return &Hook{
		path: path,
	}, nil
}

// Hook implements log.Hook and multiplexes log messages
// both to stderr and a log file.
// The console output is limited to warning level and above
// while logging to file logs at all levels.
type Hook struct {
	path string
}

func (r *Hook) Fire(entry *log.Entry) error {
	msg, err := entry.String()
	if err != nil {
		return trace.Wrap(err)
	}

	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, defaults.SharedReadWriteMask)
	if err != nil {
		return trace.Wrap(err)
	}
	defer f.Close()

	_, err = f.WriteString(msg)
	return trace.Wrap(err)
}

func (r *Hook) Levels() []log.Level {
	return log.AllLevels
}
