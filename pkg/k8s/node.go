/*
 * Copyright (c) 2022 NetLOX Inc
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
	"errors"
	"fmt"
	tk "github.com/loxilb-io/loxilib"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"net"
	"time"
)

// GetNodeAddr gets the available IP address of a Node.
// GetNodeAddr will first try to get the NodeInternalIP, then try to get the NodeExternalIP.
func GetNodeAddr(node *v1.Node) (net.IP, error) {
	addresses := make(map[v1.NodeAddressType]string)
	for _, addr := range node.Status.Addresses {
		addresses[addr.Type] = addr.Address
	}
	var ipAddrStr string
	if internalIP, ok := addresses[v1.NodeInternalIP]; ok {
		ipAddrStr = internalIP
	} else if externalIP, ok := addresses[v1.NodeExternalIP]; ok {
		ipAddrStr = externalIP
	} else {
		return nil, fmt.Errorf("node %s has neither external ip nor internal ip", node.Name)
	}
	ipAddr := net.ParseIP(ipAddrStr)
	if ipAddr == nil {
		return nil, fmt.Errorf("<%v> is not a valid ip address", ipAddrStr)
	}
	return ipAddr, nil
}

func MatchNodeinNodeList(node string, nodeMatchList []string) bool {
	for _, n := range nodeMatchList {
		if n == node {
			return true
		}
	}
	return false
}

// GetServiceLocalEndpoints - Get HostIPs of pods belonging to the given service
func GetServiceLocalEndpoints(kubeClient clientset.Interface, svc *corev1.Service, addrType string, nodeMatchList []string) ([]string, error) {
	var epList []string

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabelStr := labels.Set(svc.Spec.Selector).String()
	podList, err := kubeClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorLabelStr})
	if err != nil {
		return epList, err
	}

	epMap := make(map[string]struct{})
	for _, pod := range podList.Items {
		if pod.Status.HostIP != "" {
			hostIP := pod.Status.HostIP
			if addrType == "ipv6" && !tk.IsNetIPv6(hostIP) {
				v6found := false
				for _, ip := range pod.Status.HostIPs {
					if tk.IsNetIPv6(ip.IP) {
						hostIP = ip.IP
						v6found = true
						break
					}
				}
				if !v6found {
					continue
				}
			}
			if len(nodeMatchList) > 0 && !MatchNodeinNodeList(hostIP, nodeMatchList) {
				continue
			}
			if _, found := epMap[hostIP]; !found {
				epMap[hostIP] = struct{}{}
				epList = append(epList, hostIP)
			}
		}
	}
	if len(epList) <= 0 {
		return epList, errors.New("no active endpoints")
	}
	return epList, nil
}

// GetServicePodEndpoints - Get PodIPs of pods belonging to the given service
func GetServicePodEndpoints(kubeClient clientset.Interface, svc *corev1.Service, addrType string, nodeMatchList []string) ([]string, error) {
	var epList []string

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabelStr := labels.Set(svc.Spec.Selector).String()
	podList, err := kubeClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorLabelStr})
	if err != nil {
		return epList, err
	}

	epMap := make(map[string]struct{})
	for _, pod := range podList.Items {
		if pod.Status.PodIP != "" {
			podIP := pod.Status.PodIP
			if addrType == "ipv6" && !tk.IsNetIPv6(podIP) {
				v6found := false
				for _, ip := range pod.Status.PodIPs {
					if tk.IsNetIPv6(ip.IP) {
						podIP = ip.IP
						v6found = true
						break
					}
				}
				if !v6found {
					continue
				}
			}
			if len(nodeMatchList) > 0 && !MatchNodeinNodeList(pod.Status.HostIP, nodeMatchList) {
				continue
			}
			if _, found := epMap[podIP]; !found {
				epMap[podIP] = struct{}{}
				epList = append(epList, podIP)
			}
		}
	}
	if len(epList) <= 0 {
		return epList, errors.New("no active pod endpoints")
	}
	return epList, nil
}
