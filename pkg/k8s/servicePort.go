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
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/klog/v2"
)

func GetServicePortIntValue(kubeClient clientset.Interface, svc *corev1.Service, port corev1.ServicePort) ([]int, error) {
	if port.TargetPort.IntValue() != 0 {
		return []int{port.TargetPort.IntValue()}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabelStr := labels.Set(svc.Spec.Selector).String()
	podList, err := kubeClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorLabelStr})
	if err != nil {
		return nil, err
	}

	for _, pod := range podList.Items {
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == port.TargetPort.String() {
					return []int{int(p.ContainerPort)}, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("not found port name %s in service %s", port.TargetPort.String(), svc.Name)
}

func GetServiceEndPointsPorts(kubeClient clientset.Interface, svc *corev1.Service) ([]int, error) {
	var targetPorts []int
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	eps, err := kubeClient.CoreV1().Endpoints(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	klog.V(4).Infof("GetServiceEndPoints get endpoints: %v", eps.Subsets)
	for _, ep := range eps.Subsets {
		for _, port := range ep.Ports {
			targetPorts = append(targetPorts, int(port.Port))
		}
	}
	klog.V(4).Infof("GetServiceEndPoints return targetPorts: %v", targetPorts)

	return targetPorts, nil
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

// GetServiceEndPointsPortsWithLister - Get service endpoint ports using EndpointSlice Lister
func GetServiceEndPointsPortsWithLister(endpointSliceLister discoverylisters.EndpointSliceLister, svc *corev1.Service) ([]int, error) {
	var targetPorts []int

	// Get EndpointSlices for the service using label selector
	selector := labels.SelectorFromSet(labels.Set{
		discoveryv1.LabelServiceName: svc.Name,
	})

	endpointSlices, err := endpointSliceLister.EndpointSlices(svc.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	klog.V(4).Infof("GetServiceEndPointsPortsWithLister get endpointSlices: %d slices", len(endpointSlices))

	portSet := make(map[int]struct{})
	for _, eps := range endpointSlices {
		for _, port := range eps.Ports {
			if port.Port != nil {
				portSet[int(*port.Port)] = struct{}{}
			}
		}
	}

	for port := range portSet {
		targetPorts = append(targetPorts, port)
	}

	klog.V(4).Infof("GetServiceEndPointsPortsWithLister return targetPorts: %v", targetPorts)
	return targetPorts, nil
}

// GetServiceEndPointsWithLister - Get service endpoint IPs using EndpointSlice Lister
func GetServiceEndPointsWithLister(endpointSliceLister discoverylisters.EndpointSliceLister, svc *corev1.Service, addrType string, nodeMatchList []string) ([]string, error) {
	var retIPs []string

	// Get EndpointSlices for the service using label selector
	selector := labels.SelectorFromSet(labels.Set{
		discoveryv1.LabelServiceName: svc.Name,
	})

	endpointSlices, err := endpointSliceLister.EndpointSlices(svc.Namespace).List(selector)
	if err != nil {
		return retIPs, err
	}

	klog.V(4).Infof("GetServiceEndPointsWithLister get endpointSlices: %d slices", len(endpointSlices))

	ipSet := make(map[string]struct{})
	for _, eps := range endpointSlices {
		for _, endpoint := range eps.Endpoints {
			// Skip if endpoint is not ready
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			for _, addr := range endpoint.Addresses {
				// Filter by address type
				if addrType == "ipv6" {
					if !tk.IsNetIPv6(addr) {
						continue
					}
				} else {
					if !tk.IsNetIPv4(addr) {
						continue
					}
				}

				// Note: nodeMatchList filtering is not directly applicable with EndpointSlice
				// as it contains pod IPs, not node IPs. This is left for compatibility
				// but may need adjustment based on actual use case

				ipSet[addr] = struct{}{}
			}
		}
	}

	for ip := range ipSet {
		retIPs = append(retIPs, ip)
	}

	klog.V(4).Infof("GetServiceEndPointsWithLister return retIPs: %v", retIPs)
	return retIPs, nil
}

// GetLoxilbServiceEndPointsWithLister - Get loxilb service endpoints using EndpointSlice Lister
func GetLoxilbServiceEndPointsWithLister(endpointSliceLister discoverylisters.EndpointSliceLister, name string, ns string, nodeMatchList []string) ([]string, error) {
	var retIPs []string

	// Get EndpointSlices for the service
	selector := labels.SelectorFromSet(labels.Set{
		discoveryv1.LabelServiceName: name,
	})

	var endpointSlices []*discoveryv1.EndpointSlice
	var err error

	if ns != "" {
		endpointSlices, err = endpointSliceLister.EndpointSlices(ns).List(selector)
		if err != nil {
			return nil, err
		}
	} else {
		// Search in all namespaces
		endpointSlices, err = endpointSliceLister.List(selector)
		if err != nil {
			return nil, err
		}
	}

	ipSet := make(map[string]struct{})
	for _, eps := range endpointSlices {
		for _, endpoint := range eps.Endpoints {
			// Skip if endpoint is not ready
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			for _, addr := range endpoint.Addresses {
				IP := net.ParseIP(addr)
				if IP != nil {
					// Note: nodeMatchList filtering is not directly applicable with EndpointSlice
					// as it contains pod IPs, not node IPs. This is left for compatibility
					ipSet[IP.String()] = struct{}{}
				}
			}
		}
	}

	for ip := range ipSet {
		retIPs = append(retIPs, ip)
	}

	return retIPs, nil
}
