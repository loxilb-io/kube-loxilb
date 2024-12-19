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
	"fmt"
	"net"
	"time"

	tk "github.com/loxilb-io/loxilib"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

func GetServicePortIntValue(kubeClient clientset.Interface, svc *corev1.Service, port corev1.ServicePort) (int, error) {
	if port.TargetPort.IntValue() != 0 {
		return port.TargetPort.IntValue(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabelStr := labels.Set(svc.Spec.Selector).String()
	podList, err := kubeClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorLabelStr})
	if err != nil {
		return 0, err
	}

	for _, pod := range podList.Items {
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == port.TargetPort.String() {
					return int(p.ContainerPort), nil
				}
			}
		}
	}

	return 0, fmt.Errorf("not found port name %s in service %s", port.TargetPort.String(), svc.Name)
}

func GetServiceEndPoints(kubeClient clientset.Interface, svc *corev1.Service, addrType string, nodeMatchList []string) ([]string, error) {
	var retIPs []string
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	eps, err := kubeClient.CoreV1().Endpoints(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return retIPs, err
	}

	klog.V(4).Infof("GetServiceEndPoints get endpoints: %v", eps.Subsets)
	for _, ep := range eps.Subsets {
		for _, addr := range ep.Addresses {
			if addrType == "ipv6" {
				if !tk.IsNetIPv6(addr.IP) {
					continue
				}
			} else {
				if !tk.IsNetIPv4(addr.IP) {
					continue
				}
			}
			if len(nodeMatchList) > 0 && !MatchNodeinNodeList(addr.IP, nodeMatchList) {
				continue
			}
			retIPs = append(retIPs, addr.IP)
		}
	}
	klog.V(4).Infof("GetServiceEndPoints return retIPs: %v", retIPs)

	return retIPs, nil
}

func GetLoxilbServiceEndPoints(kubeClient clientset.Interface, name string, ns string, nodeMatchList []string) ([]string, error) {
	var retIPs []string
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	nsList, err := kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, nsItem := range nsList.Items {
		if ns != "" && nsItem.Name != ns {
			continue
		}

		eps, err := kubeClient.CoreV1().Endpoints(nsItem.Name).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}

		for _, ep := range eps.Subsets {
			for _, addr := range ep.Addresses {
				IP := net.ParseIP(addr.IP)
				if IP != nil {
					if len(nodeMatchList) > 0 && !MatchNodeinNodeList(IP.String(), nodeMatchList) {
						continue
					}
					retIPs = append(retIPs, IP.String())
				}
			}
		}
	}

	return retIPs, nil
}
