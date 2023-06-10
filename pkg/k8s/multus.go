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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"strings"
	"time"
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

func GetMultusEndpoints(kubeClient clientset.Interface, svc *corev1.Service, netList []string, addrType string) ([]string, error) {
	var epList []string

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabelStr := labels.Set(svc.Spec.Selector).String()
	podList, err := kubeClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorLabelStr})
	if err != nil {
		return epList, err
	}

	contain := func(strList []string, s string) bool {
		for _, str := range strList {
			if str == s {
				return true
			}
		}
		return false
	}

	for _, pod := range podList.Items {
		multusNetworkListStr, ok := pod.Annotations["k8s.v1.cni.cncf.io/networks"]
		if !ok {
			continue
		}

		networkStatusListStr, ok := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
		if !ok {
			return epList, errors.New("net found k8s.v1.cni.cncf.io/network-status annotation")
		}

		networkStatusList, err := UnmarshalNetworkStatus(networkStatusListStr)
		if err != nil {
			return epList, err
		}

		multusNetworkList := strings.Split(multusNetworkListStr, ",")
		for _, mNet := range multusNetworkList {
			if !contain(netList, mNet) {
				continue
			}

			netName := GetMultusNetworkName(pod.Namespace, mNet)
			for _, ns := range networkStatusList {
				if ns.Name == netName {
					if len(ns.Ips) > 0 {
						for _, ip := range ns.Ips {
							if AddrInFamily(addrType, ip) {
								epList = append(epList, ip)
							}
						}
					}
				}
			}
		}
	}

	return epList, nil
}
