/*
 * Copyright (c) 2026 NetLOX Inc
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
	"fmt"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ingressPair - a host and path loxilb-ingress keeps one rule for.
//
// loxilb-ingress serves every loxilb Ingress on one HTTP port, and on one
// HTTPS port for a host the Ingress has TLS for, whichever Gateway and
// listener it came from; the gateway under it keeps one endpoint pool for a
// host and path, Exact and Prefix alike. Two Ingresses asking for a pair take
// it from each other each time loxilb-ingress handles one of them, and the
// pair is left without a rule for up to 30 seconds when the one that has it
// goes. So the routes asking for a pair are weighed here, as the spec weighs
// them, and only the first one's Ingress asks for it.
type ingressPair struct {
	https bool
	host  string
	path  string
}

// pairClaim - a route whose Ingress asks for a pair.
type pairClaim struct {
	route   types.NamespacedName
	gateway types.NamespacedName
	exact   bool
	created metav1.Time
}

// precedes - whether c goes before o for a pair, as the spec weighs matches
// across routes: an Exact match, then the oldest route, then namespace and
// name.
func (c pairClaim) precedes(o pairClaim) bool {
	if c.exact != o.exact {
		return c.exact
	}
	if !c.created.Equal(&o.created) {
		return c.created.Before(&o.created)
	}
	return c.route.String() < o.route.String()
}

// pairOwners - for each pair the routes' Ingresses ask for, the claim that has
// it. A nil one weighs nothing: every route keeps what it asks for.
type pairOwners map[ingressPair]pairClaim

// takenBy - the claim that has pair, when it is another route's than route.
func (o pairOwners) takenBy(pair ingressPair, route types.NamespacedName) (pairClaim, bool) {
	owner, ok := o[pair]
	if !ok || owner.route == route {
		return pairClaim{}, false
	}
	return owner, true
}

// takenNote - why a path of a route's Ingress on gw is left out.
func takenNote(host, path string, owner pairClaim, gw *v1.Gateway) string {
	note := fmt.Sprintf("host %q and path %q are served for route %s", host, normalizePath(path), owner.route)
	if owner.gateway != (types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}) {
		note += fmt.Sprintf(" of gateway %s, as loxilb-ingress serves the Ingresses of every Gateway on one port", owner.gateway)
	}
	return note
}

// gatewayPort - a Gateway's port.
type gatewayPort struct {
	gateway types.NamespacedName
	port    v1.PortNumber
}

// claimedPair - a pair one of a route's Ingresses asks for.
type claimedPair struct {
	pair  ingressPair
	claim pairClaim
}

// ingressClaims - the pairs the route's Ingresses ask for, before routes are
// weighed against each other: those of the listeners it attaches to that an
// Ingress serves. assume, when set, is a port taken to be on its Gateway's
// ingress service already.
func (h *HTTPRouteManager) ingressClaims(httpRoute *v1.HTTPRoute, assume *gatewayPort) []claimedPair {
	name := types.NamespacedName{Namespace: httpRoute.Namespace, Name: httpRoute.Name}
	attaching := attachingRoute{Kind: kindHTTPRoute, Namespace: httpRoute.Namespace, Hostnames: httpRoute.Spec.Hostnames}
	nsLabels := h.namespaceLabels()

	var claims []claimedPair
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		namespace, gwName, ok := gatewayOf(httpRoute.Namespace, parentRef)
		if !ok {
			continue
		}
		gw, err := h.gatewayLister.Gateways(namespace).Get(gwName)
		if err != nil || !h.servesGateway(gw) {
			continue
		}
		gateway := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
		ingressPorts := h.ingressPortsOf(gw)
		for _, la := range attachRoute(gw, parentRef, attaching, nsLabels).Listeners {
			onIngress := ingressPorts == nil || ingressPorts[la.Listener.Port] ||
				(assume != nil && assume.gateway == gateway && assume.port == la.Listener.Port)
			if !onIngress {
				continue
			}
			ing, _, _ := h.ingressFor(httpRoute, gw, la, nil, false)
			if ing == nil {
				continue
			}
			https := la.Listener.Protocol == v1.HTTPSProtocolType
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, p := range rule.HTTP.Paths {
					claims = append(claims, claimedPair{
						pair: ingressPair{https: https, host: rule.Host, path: normalizePath(p.Path)},
						claim: pairClaim{
							route: name, gateway: gateway, created: httpRoute.CreationTimestamp,
							exact: p.PathType != nil && *p.PathType == netv1.PathTypeExact,
						},
					})
				}
			}
		}
	}
	return claims
}

// ingressPairOwners - who has each pair the routes' Ingresses ask for. assume
// is as ingressClaims'. A failure to list the routes weighs nothing, as before
// routes were weighed.
func (h *HTTPRouteManager) ingressPairOwners(assume *gatewayPort) pairOwners {
	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("unable to list HTTPRoutes: %v", err)
		return nil
	}
	owners := pairOwners{}
	for _, route := range routes {
		for _, c := range h.ingressClaims(route, assume) {
			if have, ok := owners[c.pair]; !ok || c.claim.precedes(have) {
				owners[c.pair] = c.claim
			}
		}
	}
	return owners
}

// enqueueCompetitors - queue the other routes that ask for a pair one of
// routes asks for: a route that comes may take a pair from them, and one that
// changes or goes may leave them one. routes are the old and new objects of an
// event, which need not be in the lister any more.
//
// A Gateway that changes queues its own routes; one on another Gateway that
// asks for a pair its routes have waits for its own next event.
func (h *HTTPRouteManager) enqueueCompetitors(routes ...*v1.HTTPRoute) {
	pairs := map[ingressPair]bool{}
	self := map[types.NamespacedName]bool{}
	for _, route := range routes {
		if route == nil {
			continue
		}
		self[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}] = true
		for _, c := range h.ingressClaims(route, nil) {
			pairs[c.pair] = true
		}
	}
	if len(pairs) == 0 {
		return
	}

	others, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("unable to list HTTPRoutes: %v", err)
		return
	}
	for _, other := range others {
		if self[types.NamespacedName{Namespace: other.Namespace, Name: other.Name}] {
			continue
		}
		for _, c := range h.ingressClaims(other, nil) {
			if pairs[c.pair] {
				h.queue.Add(HTTPRouteQueueEntry{other.Namespace, other.Name})
				break
			}
		}
	}
}
