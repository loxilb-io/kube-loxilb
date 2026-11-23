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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/common"
)

// Direct routes: HTTPRoutes served on a Gateway listener as rules of
// loxilb-inference-gateway, instead of through an Ingress and loxilb-ingress.
// Each rule is a generated Service, which the load-balancer manager makes
// into a host and path rule on the Gateway's address and the listener's port
// (pkg/agent/manager/loadbalancer/gateway_route.go). The rules of a port are
// worked out from every route on it at once, because two routes can ask for
// the same host and path and only one can have it.
//
// The keys below are the ones the load-balancer manager reads; they are kept
// in step with gateway_route.go there, which this package does not import.
const (
	gatewayRouteLabel                     = "loxilb.io/gateway-route"
	gatewayRouteHostAnnotation            = "loxilb.io/gateway-route-host"
	gatewayRoutePathAnnotation            = "loxilb.io/gateway-route-path"
	gatewayRoutePathMatchAnnotation       = "loxilb.io/gateway-route-path-match"
	gatewayRouteBackendProtocolAnnotation = "loxilb.io/gateway-route-backend-protocol"
	gatewayRouteBackendAnnotation         = "loxilb.io/gateway-route-backend"
	gatewayRouteBackendPortAnnotation     = "loxilb.io/gateway-route-backend-port"
	// gatewayRouteInvalidBackendAnnotation - why a rule's backend is
	// invalid, in place of the two above: the rule is answered 500.
	gatewayRouteInvalidBackendAnnotation = "loxilb.io/gateway-route-invalid-backend"

	// h2cAppProtocol - a backend Service port's appProtocol for cleartext
	// HTTP/2.
	h2cAppProtocol = "kubernetes.io/h2c"
)

// DirectPeers - whether every loxilb peer can serve direct routes; whether
// that is known, which it is not while a peer has not said what it is; and
// why not when one cannot or it is not known.
type DirectPeers func() (ok, known bool, reason string)

// RulesWithoutEndpoints - whether every loxilb peer takes a direct rule
// without endpoints, and answers the requests it matches itself. A rule whose
// backend is invalid is served so only then, answered 500 as the spec
// requires; otherwise it is left out, and its requests go to another rule.
type RulesWithoutEndpoints func() bool

// ReleasePorts - hand the TCP ports of a Gateway's ingress Service its direct
// routes serve to them, all of them each time: the load-balancer manager takes
// their rules off every peer. Reports which of them no peer has a rule for.
type ReleasePorts func(namespace, name string, ports []int32) (map[int32]bool, error)

// directPortsOf - the HTTP ports of gw served directly, and for each other
// HTTP or HTTPS port why it is not. held - the ports that would be served
// directly but for a peer that has not said what it is yet.
//
// A port is direct when every peer is the gateway, the Gateway's address is
// IPv4 - the gateway proxies IPv4 only, and takes an IPv6 rule only to drop
// its traffic - every listener on it is HTTP, and no InferencePool serves a
// listener on it. HTTPS waits for the gateway to pick wildcard certificates
// by SNI and to keep hostnames to their certificates.
func directPortsOf(gw *v1.Gateway, claimed map[v1.SectionName]bool, peers DirectPeers) (direct map[v1.PortNumber]bool, why map[v1.PortNumber]string, held map[v1.PortNumber]bool) {
	direct = map[v1.PortNumber]bool{}
	why = map[v1.PortNumber]string{}
	held = map[v1.PortNumber]bool{}

	addressed := len(gw.Spec.Addresses) > 0 && net.ParseIP(gw.Spec.Addresses[0].Value).To4() != nil
	gatewayWide := ""
	ok, known, reason := peers()
	switch {
	case !ok:
		gatewayWide = reason
	case len(gw.Spec.Addresses) == 0:
		gatewayWide = "the gateway has no address yet"
	case !addressed:
		gatewayWide = "the gateway address is not IPv4"
	}
	if !ok && !known && addressed {
		ruledOut := map[v1.PortNumber]bool{}
		for _, l := range gw.Spec.Listeners {
			switch {
			case l.Protocol == v1.HTTPProtocolType && !claimed[l.Name]:
				held[l.Port] = true
			case l.Protocol == v1.HTTPProtocolType || l.Protocol == v1.HTTPSProtocolType:
				ruledOut[l.Port] = true
			}
		}
		for port := range ruledOut {
			delete(held, port)
		}
	}

	for _, l := range gw.Spec.Listeners {
		if l.Protocol != v1.HTTPProtocolType && l.Protocol != v1.HTTPSProtocolType {
			continue
		}
		port := l.Port
		if _, decided := why[port]; decided {
			continue
		}
		switch {
		case gatewayWide != "":
			why[port] = gatewayWide
		case l.Protocol == v1.HTTPSProtocolType:
			why[port] = fmt.Sprintf("listener %s is HTTPS", l.Name)
		case claimed[l.Name]:
			why[port] = fmt.Sprintf("listener %s is served by an InferencePool", l.Name)
		default:
			direct[port] = true
			continue
		}
		delete(direct, port)
	}
	// A port where one listener ruled it out stays out, whichever came first.
	for port := range why {
		delete(direct, port)
	}
	return direct, why, held
}

// directRule - one rule of a direct port.
type directRule struct {
	port            v1.PortNumber
	host            string
	path            string
	exact           bool
	backend         types.NamespacedName
	backendPort     int32
	backendProtocol string
	route           types.NamespacedName
	// invalidBackend - why the rule's backend is invalid; it has none then,
	// and is answered 500.
	invalidBackend string
}

func (r directRule) key() string {
	return r.host + "|" + r.path
}

// directInputs - what the rules of a port are worked out from.
type directInputs struct {
	routes   []*v1.HTTPRoute
	nsLabels namespaceLabels
	grants   *ReferenceGrants
	// services - for the appProtocol of a backend's port; nil reads every
	// backend as HTTP/1.
	services corev1lister.ServiceLister
	// withoutEndpoints - the peers take a rule without endpoints
	// (RulesWithoutEndpoints).
	withoutEndpoints bool
}

// directRefusals - why parts of routes are not served on a direct port, by
// route.
type directRefusals map[types.NamespacedName][]string

func (r directRefusals) add(route types.NamespacedName, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	for _, m := range r[route] {
		if m == msg {
			return
		}
	}
	r[route] = append(r[route], msg)
}

// directCandidate - a rule a route asks for, before routes are weighed
// against each other.
type directCandidate struct {
	rule      directRule
	created   metav1.Time
	ruleIndex int
	match     int
}

// normalizePath - a path as the gateway compares it: no trailing slash, as
// its lookup shortens a request path at slashes and would never meet one.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}

// directRules - the rules of gw's port and why parts of the routes on it are
// not served there.
//
// What a rule cannot hold yet is left out, not served more broadly: a match
// on headers, query or method, a filter, more than one backend with weight,
// a regular expression path, a wildcard hostname. Two routes asking for the
// same host and path - Exact and Prefix alike, since the gateway keeps one
// endpoint pool for a pair - go by the spec's precedence: an Exact match,
// then the oldest route, then namespace and name, then the rule's order.
// Every rule of a port shares one backend protocol, the one its first rule
// has.
func directRules(gw *v1.Gateway, port v1.PortNumber, in directInputs) ([]directRule, directRefusals) {
	refusals := directRefusals{}
	var candidates []directCandidate

	for _, route := range in.routes {
		name := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		attaching := attachingRoute{Kind: kindHTTPRoute, Namespace: route.Namespace, Hostnames: route.Spec.Hostnames}

		var hosts []string
		seen := map[string]bool{}
		for _, ref := range route.Spec.ParentRefs {
			namespace, gwName, ok := gatewayOf(route.Namespace, ref)
			if !ok || namespace != gw.Namespace || gwName != gw.Name {
				continue
			}
			for _, la := range attachRoute(gw, ref, attaching, in.nsLabels).Listeners {
				if la.Listener.Port != port {
					continue
				}
				served, ok := servedHostnames(gw, la.Listener, la.Hostnames)
				if !ok {
					continue
				}
				if served == nil {
					served = []v1.Hostname{""}
				}
				for _, h := range served {
					host := strings.ToLower(string(h))
					if strings.HasPrefix(host, "*.") {
						refusals.add(name, "hostname %s is a wildcard, which is not served directly yet", host)
						continue
					}
					if !seen[host] {
						seen[host] = true
						hosts = append(hosts, host)
					}
				}
			}
		}
		if len(hosts) == 0 {
			continue
		}

		for ri, rule := range route.Spec.Rules {
			backend, backendPort, protocol, why, invalid := directBackend(route, rule, in)
			invalidBackend := ""
			if why != "" {
				if !invalid || !in.withoutEndpoints {
					refusals.add(name, "rule %d: %s", ri, why)
					continue
				}
				// Its requests are answered 500, as the spec requires, by a
				// rule without endpoints.
				invalidBackend = why
			}
			if len(rule.Filters) > 0 {
				refusals.add(name, "rule %d has filters, which are not applied directly yet", ri)
				continue
			}

			matches := rule.Matches
			if len(matches) == 0 {
				matches = []v1.HTTPRouteMatch{{}}
			}
			for mi, match := range matches {
				if len(match.Headers) > 0 || len(match.QueryParams) > 0 || match.Method != nil {
					refusals.add(name, "rule %d matches on headers, query parameters or method, which are not matched directly yet", ri)
					continue
				}
				path, exact := "/", false
				if match.Path != nil {
					if match.Path.Type != nil {
						switch *match.Path.Type {
						case v1.PathMatchExact:
							exact = true
						case v1.PathMatchRegularExpression:
							refusals.add(name, "rule %d matches a regular expression path, which is not matched directly", ri)
							continue
						}
					}
					if match.Path.Value != nil {
						path = *match.Path.Value
					}
				}
				path = normalizePath(path)

				for _, host := range hosts {
					candidates = append(candidates, directCandidate{
						rule: directRule{
							port: port, host: host, path: path, exact: exact,
							backend: backend, backendPort: backendPort, backendProtocol: protocol, route: name,
							invalidBackend: invalidBackend,
						},
						created: route.CreationTimestamp, ruleIndex: ri, match: mi,
					})
				}
			}
		}
	}

	precedes := func(a, b directCandidate) bool {
		if a.rule.exact != b.rule.exact {
			return a.rule.exact
		}
		if !a.created.Equal(&b.created) {
			return a.created.Before(&b.created)
		}
		if a.rule.route != b.rule.route {
			return a.rule.route.String() < b.rule.route.String()
		}
		if a.ruleIndex != b.ruleIndex {
			return a.ruleIndex < b.ruleIndex
		}
		return a.match < b.match
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rule.key() != candidates[j].rule.key() {
			return candidates[i].rule.key() < candidates[j].rule.key()
		}
		return precedes(candidates[i], candidates[j])
	})

	var winners []directCandidate
	for _, c := range candidates {
		if len(winners) > 0 && winners[len(winners)-1].rule.key() == c.rule.key() {
			w := winners[len(winners)-1].rule
			if w.route != c.rule.route || w.backend != c.rule.backend || w.backendPort != c.rule.backendPort ||
				w.exact != c.rule.exact || w.invalidBackend != c.rule.invalidBackend {
				refusals.add(c.rule.route, "host %q and path %q are served for route %s", c.rule.host, c.rule.path, w.route)
			}
			continue
		}
		winners = append(winners, c)
	}

	// The listener's backend protocol is its first rule's: oldest route first.
	sort.SliceStable(winners, func(i, j int) bool {
		a, b := winners[i], winners[j]
		if !a.created.Equal(&b.created) {
			return a.created.Before(&b.created)
		}
		if a.rule.route != b.rule.route {
			return a.rule.route.String() < b.rule.route.String()
		}
		return a.rule.key() < b.rule.key()
	})
	// A rule with an invalid backend has nothing to speak to, and takes the
	// listener's protocol.
	protocol := ""
	for _, c := range winners {
		if c.rule.invalidBackend == "" {
			protocol = c.rule.backendProtocol
			break
		}
	}
	if protocol == "" {
		protocol = "http1"
	}
	var rules []directRule
	for _, c := range winners {
		if c.rule.invalidBackend != "" {
			c.rule.backendProtocol = protocol
		} else if c.rule.backendProtocol != protocol {
			refusals.add(c.rule.route, "backend %s speaks %s on a listener whose rules speak %s", c.rule.backend, c.rule.backendProtocol, protocol)
			continue
		}
		rules = append(rules, c.rule)
	}
	return rules, refusals
}

// directBackend - the one backend of a rule a direct rule can carry, or why
// there is none, and whether that is because the backend is invalid: not a
// kind this controller serves, or not one the route may reach. The spec
// answers such a rule 500; the others cannot be carried yet.
func directBackend(route *v1.HTTPRoute, rule v1.HTTPRouteRule, in directInputs) (types.NamespacedName, int32, string, string, bool) {
	var effective []v1.HTTPBackendRef
	for _, ref := range rule.BackendRefs {
		if ref.Weight == nil || *ref.Weight > 0 {
			effective = append(effective, ref)
		}
	}
	switch len(effective) {
	case 0:
		return types.NamespacedName{}, 0, "", "it has no backend with a weight", false
	case 1:
	default:
		return types.NamespacedName{}, 0, "", "it splits traffic between backends, which is not done directly yet", false
	}

	ref := effective[0].BackendObjectReference
	switch classifyBackendRef(ref) {
	case backendService:
	case backendInferencePool:
		// Not invalid: the InferencePool manager serves it, and takes the
		// listener off the direct ports once it has claimed it.
		return types.NamespacedName{}, 0, "", fmt.Sprintf("backend %s is not a Service", backendRefDescription(ref)), false
	default:
		return types.NamespacedName{}, 0, "", fmt.Sprintf("backend %s is not a Service", backendRefDescription(ref)), true
	}
	if ref.Port == nil {
		return types.NamespacedName{}, 0, "", fmt.Sprintf("backend %s has no port", ref.Name), true
	}
	backend := types.NamespacedName{Namespace: route.Namespace, Name: string(ref.Name)}
	if ref.Namespace != nil {
		backend.Namespace = string(*ref.Namespace)
	}
	if !in.grants.Permits(
		grantReference{Group: v1.GroupName, Kind: kindHTTPRoute, Namespace: route.Namespace},
		grantReference{Group: "", Kind: "Service", Namespace: backend.Namespace, Name: backend.Name},
	) {
		return types.NamespacedName{}, 0, "", fmt.Sprintf("no ReferenceGrant in namespace %s permits Service %s", backend.Namespace, backend.Name), true
	}

	protocol := "http1"
	if in.services != nil {
		if svc, err := in.services.Services(backend.Namespace).Get(backend.Name); err == nil {
			for _, p := range svc.Spec.Ports {
				if p.Port == int32(*ref.Port) && p.AppProtocol != nil && *p.AppProtocol == h2cAppProtocol {
					protocol = "http2"
				}
			}
		}
	}
	return backend, int32(*ref.Port), protocol, "", false
}

// directServiceName - the generated Service of a rule: the Gateway's name and
// a hash of the rule's place, as a DNS label.
func directServiceName(gateway string, port v1.PortNumber, host, path string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{gateway, strconv.Itoa(int(port)), host, path}, "\x00")))
	suffix := hex.EncodeToString(sum[:6])

	base := strings.ToLower(strings.ReplaceAll(gateway, ".", "-"))
	if len(base) > 63-len(suffix)-1 {
		base = base[:63-len(suffix)-1]
	}
	base = strings.Trim(base, "-")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "g" + base
		if len(base) > 63-len(suffix)-1 {
			base = strings.TrimRight(base[:63-len(suffix)-1], "-")
		}
	}
	return base + "-" + suffix
}

// directService - the generated Service of a rule.
func directService(gw *v1.Gateway, rule directRule, lbClass, provider string) *corev1.Service {
	match := "prefix"
	if rule.exact {
		match = "exact"
	}
	annotations := map[string]string{
		lbModeAnnotation:                      lbModeFullProxy,
		gatewayRouteHostAnnotation:            rule.host,
		gatewayRoutePathAnnotation:            rule.path,
		gatewayRoutePathMatchAnnotation:       match,
		gatewayRouteBackendProtocolAnnotation: rule.backendProtocol,
		"gateway-api-controller":              provider,
		"parent-gateway":                      gw.Name,
		"parent-http-route":                   rule.route.String(),
	}
	if rule.invalidBackend != "" {
		annotations[gatewayRouteInvalidBackendAnnotation] = rule.invalidBackend
	} else {
		annotations[gatewayRouteBackendAnnotation] = rule.backend.String()
		annotations[gatewayRouteBackendPortAnnotation] = strconv.Itoa(int(rule.backendPort))
	}
	allocateNodePorts := false
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: gw.Namespace,
			Name:      directServiceName(gw.Name, rule.port, rule.host, rule.path),
			Labels: map[string]string{
				gatewayRouteLabel: "true",
				"implementation":  implementation,
			},
			Annotations: annotations,
			// The load-balancer manager trusts the rule because the Gateway
			// controls the Service.
			OwnerReferences: gatewayControllerRef(gw),
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass:             &lbClass,
			LoadBalancerIP:                gw.Spec.Addresses[0].Value,
			AllocateLoadBalancerNodePorts: &allocateNodePorts,
			Ports: []corev1.ServicePort{{
				Name: "http", Port: int32(rule.port), Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(int32(rule.port)),
			}},
		},
	}
}

// gatewayControllerRef - the owner reference that makes gw the controller of
// a Service. No blockOwnerDeletion: setting it needs the right to the
// Gateway's finalizers.
func gatewayControllerRef(gw *v1.Gateway) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: v1.GroupVersion.String(), Kind: "Gateway", Name: gw.Name, UID: gw.UID, Controller: &controller,
	}}
}

// controlledBy - whether gw controls svc.
func controlledBy(svc *corev1.Service, gw *v1.Gateway) bool {
	owner := metav1.GetControllerOf(svc)
	return owner != nil && owner.Kind == "Gateway" && owner.UID == gw.UID
}

// installed - whether the load-balancer manager has put the Service's rule on
// its peers: it reports the address in the Service's status then, as an IP or,
// in a zone, as a hostname of the zone and the address.
func installed(svc *corev1.Service, zone string) bool {
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		addr := ing.IP
		if addr == "" && ing.Hostname != "" {
			addr = common.ParseIngressHostname(ing.Hostname, zone)
		}
		if addr != "" && addr == svc.Spec.LoadBalancerIP {
			return true
		}
	}
	return false
}

// directServices - the generated Services gw controls, by name.
func (gm *GatewayManager) directServices(gw *v1.Gateway) (map[string]*corev1.Service, error) {
	owned := map[string]*corev1.Service{}
	if gm.serviceLister == nil {
		return owned, nil
	}
	list, err := gm.serviceLister.Services(gw.Namespace).List(labels.SelectorFromSet(labels.Set{gatewayRouteLabel: "true"}))
	if err != nil {
		return nil, err
	}
	for _, svc := range list {
		if controlledBy(svc, gw) {
			owned[svc.Name] = svc
		}
	}
	return owned, nil
}

// applyDirectRoutes - create or update the generated Services of gw's direct
// ports. desired names every generated Service gw should have; ready, the
// direct ports whose rules are all on the peers, which the ingress service
// can leave.
func (gm *GatewayManager) applyDirectRoutes(ctx context.Context, gw *v1.Gateway, direct map[v1.PortNumber]bool) (desired map[string]bool, ready map[v1.PortNumber]bool, err error) {
	desired, ready = map[string]bool{}, map[v1.PortNumber]bool{}
	if len(direct) == 0 {
		return desired, ready, nil
	}

	existing, err := gm.directServices(gw)
	if err != nil {
		return nil, nil, err
	}
	routes, err := gm.httpRouteLister.List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}
	in := directInputs{
		routes: routes, nsLabels: gm.namespaceLabels(), grants: gm.referenceGrants, services: gm.serviceLister,
		withoutEndpoints: gm.rulesWithoutEndpoints != nil && gm.rulesWithoutEndpoints(),
	}

	var errs []error
	services := gm.kubeClient.CoreV1().Services(gw.Namespace)
	for port := range direct {
		rules, _ := directRules(gw, port, in)
		portReady := true
		for _, rule := range rules {
			want := directService(gw, rule, gm.networkConfig.LoxilbLoadBalancerClass, gm.networkConfig.LoxilbGatewayClass)
			desired[want.Name] = true

			have, ok := existing[want.Name]
			if !ok {
				portReady = false
				if _, err := services.Create(ctx, want, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
					errs = append(errs, fmt.Errorf("gateway %s/%s: create service %s: %w", gw.Namespace, gw.Name, want.Name, err))
				}
				continue
			}

			updated := have.DeepCopy()
			updated.Labels, updated.Annotations, updated.OwnerReferences = want.Labels, want.Annotations, want.OwnerReferences
			updated.Spec.Type, updated.Spec.LoadBalancerClass = want.Spec.Type, want.Spec.LoadBalancerClass
			updated.Spec.LoadBalancerIP, updated.Spec.AllocateLoadBalancerNodePorts = want.Spec.LoadBalancerIP, want.Spec.AllocateLoadBalancerNodePorts
			updated.Spec.Ports = want.Spec.Ports
			if !equality.Semantic.DeepEqual(have.ObjectMeta, updated.ObjectMeta) || !equality.Semantic.DeepEqual(have.Spec, updated.Spec) {
				portReady = false
				if _, err := services.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
					errs = append(errs, fmt.Errorf("gateway %s/%s: update service %s: %w", gw.Namespace, gw.Name, want.Name, err))
				}
				continue
			}
			if !installed(have, gm.networkConfig.Zone) {
				portReady = false
			}
		}
		if portReady {
			ready[port] = true
		}
	}
	return desired, ready, utilerrors.NewAggregate(errs)
}

// pruneDirectRoutes - delete the generated Services gw controls that desired
// does not name, but those on the ports in keep: ports on their way back to
// the ingress service, whose L4 rule does not serve them yet.
func (gm *GatewayManager) pruneDirectRoutes(ctx context.Context, gw *v1.Gateway, desired map[string]bool, keep map[v1.PortNumber]bool) error {
	existing, err := gm.directServices(gw)
	if err != nil {
		return err
	}
	var errs []error
	for name, svc := range existing {
		if desired[name] {
			continue
		}
		if len(svc.Spec.Ports) > 0 && keep[v1.PortNumber(svc.Spec.Ports[0].Port)] {
			continue
		}
		if err := gm.kubeClient.CoreV1().Services(gw.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("gateway %s/%s: delete service %s: %w", gw.Namespace, gw.Name, name, err))
			continue
		}
		klog.Infof("gateway %s/%s: direct route service %s is deleted", gw.Namespace, gw.Name, name)
	}
	return utilerrors.NewAggregate(errs)
}
