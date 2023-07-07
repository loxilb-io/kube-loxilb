// Copyright 2023 Netlox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package log

import (
	"flag"

	"k8s.io/klog/v2"
)

const logVerbosityFlag = "v"

// GetCurrentLogLevel returns the current log verbosity level.
func GetCurrentLogLevel() string {
	return flag.Lookup(logVerbosityFlag).Value.String()
}

// InitLogLevel sets the log verbosity level when init time
func InitLogLevel() error {
	level := GetCurrentLogLevel()

	var l klog.Level
	err := l.Set(level)
	if err != nil {
		return err
	}

	klog.Infof("Set log level to %s", level)
	return nil
}

// SetLogLevel sets the log verbosity level. level must be a string
// representation of a decimal integer.
func SetLogLevel(level string) error {
	oldLevel := GetCurrentLogLevel()
	if oldLevel == level {
		return nil
	}

	var l klog.Level
	err := l.Set(level)
	if err != nil {
		return err
	}
	klog.Infof("Changed log level from %s to %s", oldLevel, level)
	return nil

}
