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

package gatewayapi

import (
	"context"
	"fmt"
	"net"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A Gateway's ingress service carries its listeners to loxilb-ingress, which
// runs where it was deployed: its manifest puts it in kube-system. A Service's
// selector picks pods in the Service's own namespace only, and the ingress
// service is in the Gateway's, so selecting loxilb-ingress left the ingress
// service of a Gateway anywhere else without endpoints. The ingress service
// has no selector; this manager writes its EndpointSlices, naming the
// loxilb-ingress pods wherever they run.

const (
	// loxilbIngressSelector - the label loxilb-ingress's manifest gives its
	// pods.
	loxilbIngressSelector = "app=loxilb-ingress"
	// ingressSliceManager - the endpointslice.kubernetes.io/managed-by value
	// of the slices written here. Any value but the endpoint slice
	// controller's keeps that controller off them.
	ingressSliceManager = "kube-loxilb"
)

// isLoxilbIngressPod - whether obj is a loxilb-ingress pod, or the last state
// of one deleted.
func isLoxilbIngressPod(obj interface{}) bool {
	if deleted, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deleted.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	selector, _ := labels.Parse(loxilbIngressSelector)
	return selector.Matches(labels.Set(pod.Labels))
}

// loxilbIngressPodChanged - queue every Gateway when a loxilb-ingress pod
// changes what its ingress services' endpoints say: its addresses, its
// readiness, or that it is going.
func (gm *GatewayManager) loxilbIngressPodChanged(old, cur interface{}) {
	before, ok1 := old.(*corev1.Pod)
	after, ok2 := cur.(*corev1.Pod)
	if ok1 && ok2 && podReady(before) == podReady(after) && before.Status.PodIP == after.Status.PodIP &&
		equality.Semantic.DeepEqual(before.Status.PodIPs, after.Status.PodIPs) &&
		(before.DeletionTimestamp == nil) == (after.DeletionTimestamp == nil) && before.Spec.NodeName == after.Spec.NodeName {
		return
	}
	gm.EnqueueAll()
}

// loxilbIngressPods - the loxilb-ingress pods, in every namespace.
func (gm *GatewayManager) loxilbIngressPods() []*corev1.Pod {
	if gm.podLister == nil {
		return nil
	}
	selector, _ := labels.Parse(loxilbIngressSelector)
	pods, err := gm.podLister.List(selector)
	if err != nil {
		klog.Errorf("unable to list loxilb-ingress pods: %v", err)
		return nil
	}
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Namespace+"/"+pods[i].Name < pods[j].Namespace+"/"+pods[j].Name
	})
	return pods
}

func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ingressSliceName - the name of svc's EndpointSlice for an address family.
func ingressSliceName(svc *corev1.Service, family discoveryv1.AddressType) string {
	if family == discoveryv1.AddressTypeIPv6 {
		return svc.Name + "-loxilb-ingress-ipv6"
	}
	return svc.Name + "-loxilb-ingress"
}

// ingressSlices - the EndpointSlices the ingress service svc of gw should
// have: one per address family of the loxilb-ingress pods, IPv4 always, each
// pod with the ingress service's target ports.
func (gm *GatewayManager) ingressSlices(gw *v1.Gateway, svc *corev1.Service) []*discoveryv1.EndpointSlice {
	var ports []discoveryv1.EndpointPort
	for _, p := range svc.Spec.Ports {
		name, protocol, port := p.Name, p.Protocol, p.TargetPort.IntVal
		ports = append(ports, discoveryv1.EndpointPort{Name: &name, Protocol: &protocol, Port: &port})
	}

	byFamily := map[discoveryv1.AddressType][]discoveryv1.Endpoint{}
	for _, pod := range gm.loxilbIngressPods() {
		ready := podReady(pod)
		terminating := pod.DeletionTimestamp != nil
		ips := pod.Status.PodIPs
		if len(ips) == 0 && pod.Status.PodIP != "" {
			ips = []corev1.PodIP{{IP: pod.Status.PodIP}}
		}
		for _, ip := range ips {
			parsed := net.ParseIP(ip.IP)
			if parsed == nil {
				continue
			}
			family := discoveryv1.AddressTypeIPv4
			if parsed.To4() == nil {
				family = discoveryv1.AddressTypeIPv6
			}
			node := pod.Spec.NodeName
			byFamily[family] = append(byFamily[family], discoveryv1.Endpoint{
				Addresses:  []string{ip.IP},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready, Serving: &ready, Terminating: &terminating},
				NodeName:   &node,
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
				},
			})
		}
	}
	if _, ok := byFamily[discoveryv1.AddressTypeIPv4]; !ok {
		byFamily[discoveryv1.AddressTypeIPv4] = nil
	}

	var slices []*discoveryv1.EndpointSlice
	for _, family := range []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6} {
		endpoints, ok := byFamily[family]
		if !ok {
			continue
		}
		slices = append(slices, &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ingressSliceName(svc, family),
				Namespace: svc.Namespace,
				Labels: map[string]string{
					discoveryv1.LabelServiceName: svc.Name,
					discoveryv1.LabelManagedBy:   ingressSliceManager,
				},
				OwnerReferences: gatewayControllerRef(gw),
			},
			AddressType: family,
			Endpoints:   endpoints,
			Ports:       ports,
		})
	}
	return slices
}

// ownIngressSlices - the EndpointSlices this manager wrote for the ingress
// service named svcName in namespace.
func (gm *GatewayManager) ownIngressSlices(ctx context.Context, namespace, svcName string) ([]*discoveryv1.EndpointSlice, error) {
	selector := labels.SelectorFromSet(labels.Set{
		discoveryv1.LabelServiceName: svcName,
		discoveryv1.LabelManagedBy:   ingressSliceManager,
	})
	if gm.endpointSliceLister != nil {
		return gm.endpointSliceLister.EndpointSlices(namespace).List(selector)
	}
	list, err := gm.kubeClient.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	slices := make([]*discoveryv1.EndpointSlice, 0, len(list.Items))
	for i := range list.Items {
		slices = append(slices, &list.Items[i])
	}
	return slices, nil
}

// syncIngressEndpoints - bring the EndpointSlices of gw's ingress service in
// line with the loxilb-ingress pods; with no ingress service (svc nil), take
// them away.
func (gm *GatewayManager) syncIngressEndpoints(ctx context.Context, gw *v1.Gateway, svc *corev1.Service) error {
	existing, err := gm.ownIngressSlices(ctx, gw.Namespace, fmt.Sprintf("%s-ingress-service", gw.Name))
	if err != nil {
		return fmt.Errorf("gateway %s/%s: the EndpointSlices of its ingress service: %w", gw.Namespace, gw.Name, err)
	}
	have := make(map[string]*discoveryv1.EndpointSlice, len(existing))
	for _, s := range existing {
		have[s.Name] = s
	}

	var errs []error
	slices := gm.kubeClient.DiscoveryV1().EndpointSlices(gw.Namespace)
	want := map[string]bool{}
	if svc != nil {
		for _, slice := range gm.ingressSlices(gw, svc) {
			want[slice.Name] = true
			cur, ok := have[slice.Name]
			if !ok {
				if _, err := slices.Create(ctx, slice, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
					errs = append(errs, fmt.Errorf("create EndpointSlice %s: %w", slice.Name, err))
				}
				continue
			}
			if equality.Semantic.DeepEqual(cur.Endpoints, slice.Endpoints) && equality.Semantic.DeepEqual(cur.Ports, slice.Ports) &&
				equality.Semantic.DeepEqual(cur.Labels, slice.Labels) && equality.Semantic.DeepEqual(cur.OwnerReferences, slice.OwnerReferences) {
				continue
			}
			updated := cur.DeepCopy()
			updated.Labels, updated.OwnerReferences = slice.Labels, slice.OwnerReferences
			updated.Endpoints, updated.Ports = slice.Endpoints, slice.Ports
			if _, err := slices.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("update EndpointSlice %s: %w", slice.Name, err))
			}
		}
	}
	for name := range have {
		if want[name] {
			continue
		}
		if err := slices.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete EndpointSlice %s: %w", name, err))
		}
	}
	if err := utilerrors.NewAggregate(errs); err != nil {
		return fmt.Errorf("gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	return nil
}
