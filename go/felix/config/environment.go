// Copyright (c) 2016 Tigera, Inc. All rights reserved.
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

package config

import (
	"github.com/golang/glog"
	"strings"
)

func LoadConfigFromEnvironment(environ []string) map[string]string {
	result := make(map[string]string)
	for _, kv := range environ {
		splits := strings.SplitN(kv, "=", 2)
		if len(splits) < 2 {
			glog.Warningf("Ignoring malformed environment variable: %#v",
				kv)
			continue
		}
		key := strings.ToLower(splits[0])
		value := splits[1]
		if strings.Index(key, "felix_") == 0 {
			splits = strings.SplitN(key, "_", 2)
			paramName := splits[1]
			glog.V(2).Infof("Found felix environment variable: %#v=%#v",
				paramName, value)
			result[paramName] = value
		}
	}
	return result
}
