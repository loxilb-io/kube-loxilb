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
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Route attachment: which listeners of a Gateway a route's parentRef attaches
// to, with which hostnames, and why none when it attaches to none. It reads
// only the objects it is handed, so every route manager and the status they
// report come to the same answer.

const (
	kindHTTPRoute v1.Kind = "HTTPRoute"
	kindTCPRoute  v1.Kind = "TCPRoute"
	kindUDPRoute  v1.Kind = "UDPRoute"
)

// gatewayOf - the Gateway a parentRef names, by namespace and name. False
// when it names something other than a Gateway. A parentRef without a
// namespace names one in the route's own.
func gatewayOf(routeNamespace string, ref v1.ParentReference) (namespace, name string, ok bool) {
	if ref.Group != nil && *ref.Group != v1.GroupName {
		return "", "", false
	}
	if ref.Kind != nil && *ref.Kind != "Gateway" {
		return "", "", false
	}

	namespace = routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}
	return namespace, string(ref.Name), true
}

// protocolRouteKinds - the route kinds this controller serves on a listener of
// the protocol. A listener whose allowedRoutes names no kinds takes these.
// TLS has none: TLSRoute is not served, and a TCPRoute's rule would carry the
// connection without terminating it.
func protocolRouteKinds(protocol v1.ProtocolType) []v1.Kind {
	switch protocol {
	case v1.HTTPProtocolType, v1.HTTPSProtocolType:
		return []v1.Kind{kindHTTPRoute}
	case v1.TCPProtocolType:
		return []v1.Kind{kindTCPRoute}
	case v1.UDPProtocolType:
		return []v1.Kind{kindUDPRoute}
	}
	return nil
}

// listenerRouteKinds - the route kinds a listener takes, and the kinds its
// allowedRoutes names that it cannot: another group, or a kind this
// controller does not serve on that protocol. The listener reports those as
// InvalidRouteKinds; the rest still attach.
func listenerRouteKinds(l *v1.Listener) (admitted []v1.Kind, invalid []v1.RouteGroupKind) {
	supported := protocolRouteKinds(l.Protocol)
	if l.AllowedRoutes == nil || len(l.AllowedRoutes.Kinds) == 0 {
		return supported, nil
	}

	for _, rgk := range l.AllowedRoutes.Kinds {
		group := v1.GroupName
		if rgk.Group != nil {
			group = string(*rgk.Group)
		}
		if group != v1.GroupName || !slices.Contains(supported, rgk.Kind) {
			invalid = append(invalid, rgk)
			continue
		}
		if !slices.Contains(admitted, rgk.Kind) {
			admitted = append(admitted, rgk.Kind)
		}
	}
	return admitted, invalid
}

// namespaceLabels - the labels of a namespace, for a listener that selects
// the namespaces it takes routes from.
type namespaceLabels func(name string) (labels.Set, error)

// listenerTakesNamespace - whether a listener takes routes from a namespace,
// and if not, why, as the end of a sentence about the listener.
func listenerTakesNamespace(gw *v1.Gateway, l *v1.Listener, namespace string, nsLabels namespaceLabels) (bool, string) {
	from := v1.NamespacesFromSame
	var selector *metav1.LabelSelector
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil {
		if l.AllowedRoutes.Namespaces.From != nil {
			from = *l.AllowedRoutes.Namespaces.From
		}
		selector = l.AllowedRoutes.Namespaces.Selector
	}

	switch from {
	case v1.NamespacesFromAll:
		return true, ""
	case v1.NamespacesFromSame:
		if namespace == gw.Namespace {
			return true, ""
		}
		return false, fmt.Sprintf("takes routes from namespace %s only", gw.Namespace)
	case v1.NamespacesFromNone:
		return false, "takes no routes"
	case v1.NamespacesFromSelector:
		if selector == nil {
			return false, "selects namespaces without a selector"
		}
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return false, fmt.Sprintf("has an invalid namespace selector: %v", err)
		}
		if nsLabels == nil {
			return false, fmt.Sprintf("cannot read the labels of namespace %s", namespace)
		}
		set, err := nsLabels(namespace)
		if err != nil {
			return false, fmt.Sprintf("cannot read the labels of namespace %s: %v", namespace, err)
		}
		if sel.Matches(set) {
			return true, ""
		}
		return false, fmt.Sprintf("does not select namespace %s", namespace)
	}
	return false, fmt.Sprintf("takes routes from %q, which is not a known value", from)
}

// wildcardSuffix - for "*.example.com", ".example.com"; "" for a hostname
// without a wildcard label.
func wildcardSuffix(h v1.Hostname) string {
	if s := string(h); strings.HasPrefix(s, "*.") {
		return s[1:]
	}
	return ""
}

// hostnameIntersection - the hostname two hostnames both match, which is the
// more specific of the two, or false when they share none.
//
// A wildcard label is a suffix match: "*.example.com" matches
// "test.example.com" and "foo.test.example.com", but not "example.com".
// Hostnames are lowercase by the API's validation, so they compare as they
// are.
func hostnameIntersection(a, b v1.Hostname) (v1.Hostname, bool) {
	as, bs := wildcardSuffix(a), wildcardSuffix(b)
	switch {
	case as == "" && bs == "":
		return a, a == b
	case as != "" && bs == "":
		return b, strings.HasSuffix(string(b), as)
	case as == "" && bs != "":
		return a, strings.HasSuffix(string(a), bs)
	case as == bs:
		return a, true
	case strings.HasSuffix(bs, as):
		return b, true
	case strings.HasSuffix(as, bs):
		return a, true
	}
	return "", false
}

// listenerHostnames - the hostnames a route serves on a listener: nil for any
// host, and false when the listener's hostname and the route's share none.
//
// The route's hostnames that do not match the listener's are dropped, and the
// ones that do are narrowed to what both match. A route with no hostnames on
// a listener with one serves the listener's.
func listenerHostnames(l *v1.Listener, route []v1.Hostname) ([]v1.Hostname, bool) {
	if l.Hostname == nil || *l.Hostname == "" {
		if len(route) == 0 {
			return nil, true
		}
		return slices.Clone(route), true
	}
	if len(route) == 0 {
		return []v1.Hostname{*l.Hostname}, true
	}

	var hosts []v1.Hostname
	for _, h := range route {
		if host, ok := hostnameIntersection(*l.Hostname, h); ok && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts, len(hosts) > 0
}

// moreSpecificHostname - whether listener hostname a is more specific than b,
// where nil b, a listener without a hostname, is the least specific of all.
// An exact name is more specific than a wildcard, and a wildcard with the
// longer suffix more than one with the shorter. Two exact names never are:
// they are equal or match nothing in common.
func moreSpecificHostname(a v1.Hostname, b *v1.Hostname) bool {
	if b == nil || *b == "" {
		return true
	}
	as, bs := wildcardSuffix(a), wildcardSuffix(*b)
	switch {
	case as == "" && bs == "":
		return false
	case as == "":
		return true
	case bs == "":
		return false
	}
	return len(as) > len(bs)
}

// servedHostnames - those of a route's hostnames on a listener that requests
// reach it with: nil for any host, and false when none are left.
//
// A request goes to the listener on its port whose hostname matches it most
// specifically. A route hostname that a more specific listener on the same
// port matches entirely never reaches this one, whether or not the route
// attaches there too, and is left out. One the other listener matches only in
// part stays, as does any host: an Ingress rule names a single host or any,
// and cannot leave out what the other listener takes.
func servedHostnames(gw *v1.Gateway, l *v1.Listener, hosts []v1.Hostname) ([]v1.Hostname, bool) {
	if hosts == nil {
		return nil, true
	}

	var served []v1.Hostname
	for _, h := range hosts {
		if !takenByMoreSpecificListener(gw, l, h) {
			served = append(served, h)
		}
	}
	return served, len(served) > 0
}

// takenByMoreSpecificListener - whether another listener on l's port, with a
// hostname more specific than l's, matches every host h does.
func takenByMoreSpecificListener(gw *v1.Gateway, l *v1.Listener, h v1.Hostname) bool {
	for i := range gw.Spec.Listeners {
		other := &gw.Spec.Listeners[i]
		if other.Name == l.Name || other.Port != l.Port || other.Hostname == nil || *other.Hostname == "" {
			continue
		}
		if !moreSpecificHostname(*other.Hostname, l.Hostname) {
			continue
		}
		if both, ok := hostnameIntersection(*other.Hostname, h); ok && both == h {
			return true
		}
	}
	return false
}

// attachingRoute - what attachment reads of a route.
type attachingRoute struct {
	Kind      v1.Kind
	Namespace string
	// Hostnames - nil for a kind without hostnames, such as TCPRoute.
	Hostnames []v1.Hostname
}

// listenerAttachment - one listener a route attaches to.
type listenerAttachment struct {
	Listener *v1.Listener
	// Hostnames - the hostnames the route serves on this listener; nil for
	// any host.
	Hostnames []v1.Hostname
}

// routeAttachment - what one parentRef of a route comes to on its Gateway.
type routeAttachment struct {
	// Listeners - the listeners the route attaches to, in the Gateway's
	// order. The parentRef is accepted when there is at least one.
	Listeners []listenerAttachment
	// Reason, Message - why it attaches to none; empty when it attaches.
	Reason  v1.RouteConditionReason
	Message string
}

// Accepted - whether the route attaches to the Gateway through this parentRef.
func (a routeAttachment) Accepted() bool {
	return len(a.Listeners) > 0
}

// attachRoute - the listeners of gw a route attaches to through ref, a
// parentRef that names gw.
//
// The parentRef selects listeners by sectionName and port, each when set; with
// neither it selects them all. A selected listener takes the route when it
// takes the route's kind, takes routes from the route's namespace, and shares
// a hostname with it. The route attaches when one listener takes it.
//
// When none does, the reason follows how far the nearest listener got:
// NoMatchingParent when none was selected, NoMatchingListenerHostname when
// one took the route's kind and namespace but not its hostnames, and
// NotAllowedByListeners otherwise.
func attachRoute(gw *v1.Gateway, ref v1.ParentReference, route attachingRoute, nsLabels namespaceLabels) routeAttachment {
	var selected []*v1.Listener
	for i := range gw.Spec.Listeners {
		l := &gw.Spec.Listeners[i]
		if ref.SectionName != nil && l.Name != *ref.SectionName {
			continue
		}
		if ref.Port != nil && l.Port != *ref.Port {
			continue
		}
		selected = append(selected, l)
	}
	if len(selected) == 0 {
		return routeAttachment{
			Reason:  v1.RouteReasonNoMatchingParent,
			Message: noListenerMessage(gw, ref),
		}
	}

	var attached []listenerAttachment
	var refusals []string
	hostnameMiss := false
	for _, l := range selected {
		if kinds, _ := listenerRouteKinds(l); !slices.Contains(kinds, route.Kind) {
			refusals = append(refusals, fmt.Sprintf("listener %s does not take %s", l.Name, route.Kind))
			continue
		}
		if ok, why := listenerTakesNamespace(gw, l, route.Namespace, nsLabels); !ok {
			refusals = append(refusals, fmt.Sprintf("listener %s %s", l.Name, why))
			continue
		}
		hosts, ok := listenerHostnames(l, route.Hostnames)
		if !ok {
			hostnameMiss = true
			refusals = append(refusals, fmt.Sprintf("listener %s hostname %s matches none of the route's hostnames", l.Name, *l.Hostname))
			continue
		}
		attached = append(attached, listenerAttachment{Listener: l, Hostnames: hosts})
	}

	if len(attached) > 0 {
		return routeAttachment{Listeners: attached}
	}
	reason := v1.RouteReasonNotAllowedByListeners
	if hostnameMiss {
		reason = v1.RouteReasonNoMatchingListenerHostname
	}
	return routeAttachment{Reason: reason, Message: strings.Join(refusals, "; ")}
}

// noListenerMessage - why a parentRef selects no listener of gw.
func noListenerMessage(gw *v1.Gateway, ref v1.ParentReference) string {
	switch {
	case ref.SectionName != nil && ref.Port != nil:
		return fmt.Sprintf("Gateway %s/%s has no listener %s on port %d", gw.Namespace, gw.Name, *ref.SectionName, *ref.Port)
	case ref.SectionName != nil:
		return fmt.Sprintf("Gateway %s/%s has no listener %s", gw.Namespace, gw.Name, *ref.SectionName)
	case ref.Port != nil:
		return fmt.Sprintf("Gateway %s/%s has no listener on port %d", gw.Namespace, gw.Name, *ref.Port)
	}
	return fmt.Sprintf("Gateway %s/%s has no listeners", gw.Namespace, gw.Name)
}
