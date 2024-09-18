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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
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

func GetServiceEndPoints(kubeClient clientset.Interface, name string, ns string, nodeMatchList []string) ([]net.IP, error) {
	var retIPs []net.IP
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	eps, err := kubeClient.CoreV1().Endpoints(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	for _, ep := range eps.Subsets {
		for _, addr := range ep.Addresses {
			IP := net.ParseIP(addr.IP)
			if IP != nil {
				if len(nodeMatchList) > 0 && !MatchNodeinNodeList(IP.String(), nodeMatchList) {
					continue
				}
				retIPs = append(retIPs, IP)
			}
		}
	}

	return retIPs, nil
}
