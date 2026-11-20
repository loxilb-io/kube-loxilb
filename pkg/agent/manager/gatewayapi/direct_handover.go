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
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A port moves from the ingress service to the direct routes once: when all
// its rules are on the peers. Its L4 rule comes off the peers first
// (ReleasePorts), since the gateway serves it ahead of the direct rules and,
// once the port is off the ingress service, sends it to a closed node port.
// Then the port leaves the ingress service.
//
// From then on the port stays with the direct routes while it can be served
// directly: while their rules change and routes come and go, and while a peer
// has not said what it is yet, as each peer has not when kube-loxilb starts.
// Sending it back to the ingress service whenever a rule was not on the peers
// moved it there and back on every route change, and on a start with a peer
// that did not answer, it took every direct route down until that peer did.

// errHandoverPending - a port is on its way between the ingress service and
// the direct routes: going to them, its L4 rule is not off every peer yet;
// going back, loxilb-ingress or the peers do not serve it yet
// (ingress_programmed.go).
var errHandoverPending = errors.New("a port is on its way between the ingress service and the direct routes")

func isHandoverPending(err error) bool { return errors.Is(err, errHandoverPending) }

// ingressServiceName - the name of gw's ingress service.
func ingressServiceName(gw *v1.Gateway) string {
	return fmt.Sprintf("%s-ingress-service", gw.Name)
}

// handedOver - the ports among direct and held that gw's direct routes serve
// already: those its ingress service does not carry. With no ingress service,
// which there is none of once every port is direct, the ports with rule
// Services.
func (gm *GatewayManager) handedOver(gw *v1.Gateway, ingress *corev1.Service, direct, held map[v1.PortNumber]bool) map[v1.PortNumber]bool {
	taken := map[v1.PortNumber]bool{}
	candidate := func(p v1.PortNumber) bool { return direct[p] || held[p] }
	if ingress != nil {
		carried := map[v1.PortNumber]bool{}
		for _, p := range ingress.Spec.Ports {
			carried[v1.PortNumber(p.Port)] = true
		}
		for p := range direct {
			if !carried[p] {
				taken[p] = true
			}
		}
		for p := range held {
			if !carried[p] {
				taken[p] = true
			}
		}
		return taken
	}
	rules, err := gm.directServices(gw)
	if err != nil {
		return taken
	}
	for _, svc := range rules {
		if len(svc.Spec.Ports) > 0 && candidate(v1.PortNumber(svc.Spec.Ports[0].Port)) {
			taken[v1.PortNumber(svc.Spec.Ports[0].Port)] = true
		}
	}
	return taken
}

// handOver - the ports to keep off the ingress service: those taken over
// already, and the direct ones whose rules are all on the peers, once the
// load-balancer manager has their L4 rule off every peer. While one waits
// for that, errHandoverPending brings the Gateway back. The ports in hold
// stay on the ingress service, and their L4 rule off the peers.
func (gm *GatewayManager) handOver(gw *v1.Gateway, direct, taken, ready, hold map[v1.PortNumber]bool) (map[v1.PortNumber]bool, error) {
	var ports []int32
	for p := range direct {
		if taken[p] || ready[p] {
			ports = append(ports, int32(p))
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })

	off := map[v1.PortNumber]bool{}
	if gm.releasePorts == nil {
		for _, p := range ports {
			off[v1.PortNumber(p)] = true
		}
		return off, nil
	}
	release := append([]int32{}, ports...)
	for p := range hold {
		if !direct[p] {
			release = append(release, int32(p))
		}
	}
	sort.Slice(release, func(i, j int) bool { return release[i] < release[j] })
	released, err := gm.releasePorts(gw.Namespace, ingressServiceName(gw), release)
	var waiting []int32
	for _, p := range ports {
		if taken[v1.PortNumber(p)] || released[p] {
			off[v1.PortNumber(p)] = true
		} else {
			waiting = append(waiting, p)
		}
	}
	if err != nil {
		return off, fmt.Errorf("gateway %s/%s: taking the L4 rules of ports %v off the peers: %w", gw.Namespace, gw.Name, waiting, err)
	}
	if len(waiting) > 0 {
		return off, fmt.Errorf("gateway %s/%s: ports %v: %w", gw.Namespace, gw.Name, waiting, errHandoverPending)
	}
	return off, nil
}
