/*
 * Copyright (c) 2023 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package k8s

import (
	"encoding/json"
	"fmt"
	"strings"
)

type dnsIf interface{}

type networkStatus struct {
	Name  string   `json:"name"`
	Iface string   `json:"interface"`
	Ips   []string `json:"ips"`
	Mac   string   `json:"mac"`
	Dflt  bool     `json:"default"`
	Dns   dnsIf    `json:"dns"`
}

func GetMultusNetworkName(ns, name string) string {
	return strings.Join([]string{ns, name}, "/")
}

func UnmarshalNetworkStatus(ns string) ([]networkStatus, error) {
	data := []networkStatus{}
	err := json.Unmarshal([]byte(ns), &data)
	if err != nil {
		return data, err
	}

	return data, nil
}

func GetMultusNetworkStatus(ns, name string) (networkStatus, error) {
	data, err := UnmarshalNetworkStatus(ns)
	if err != nil {
		return networkStatus{}, err
	}

	for _, d := range data {
		if d.Name == name {
			return d, nil
		}
	}

	return networkStatus{}, fmt.Errorf("not found %s network", name)
}
