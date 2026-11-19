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

package loadbalancer

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwlister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// Gateway routes: Services the Gateway API managers generate to serve an
// HTTPRoute directly on a Gateway listener. Each is one rule on the Gateway's
// address and the listener's port, told apart from the others there by its
// host and path, and served by the endpoints of the route's backend Service.
//
// What the rule is comes from the annotations below. A Service that only
// carries them is not trusted with them: its rule reads another Service's
// endpoints, possibly in another namespace, and takes a share of the Gateway's
// listener, neither of which a Service an application team can write should be
// able to claim. So a gateway route has to be in the Gateway's namespace,
// controlled by that Gateway, on one of its addresses.
const (
	// gatewayRouteLabel - marks the Service as a gateway route, with the
	// value "true". The Gateway is the one its controller owner reference
	// names: a Gateway name can be longer than a label value.
	gatewayRouteLabel = "loxilb.io/gateway-route"

	gatewayRouteHostAnnotation            = "loxilb.io/gateway-route-host"
	gatewayRoutePathAnnotation            = "loxilb.io/gateway-route-path"
	gatewayRoutePathMatchAnnotation       = "loxilb.io/gateway-route-path-match"
	gatewayRouteTLSAnnotation             = "loxilb.io/gateway-route-tls"
	gatewayRouteBackendProtocolAnnotation = "loxilb.io/gateway-route-backend-protocol"
	// gatewayRouteBackendAnnotation - the backend Service, namespace/name.
	gatewayRouteBackendAnnotation = "loxilb.io/gateway-route-backend"
	// gatewayRouteBackendPortAnnotation - the port of the backend Service the
	// route names, by number.
	gatewayRouteBackendPortAnnotation = "loxilb.io/gateway-route-backend-port"

	// ReasonGatewayRouteRefused - the event reason for a gateway route that
	// is not trusted or does not read as one.
	ReasonGatewayRouteRefused = "GatewayRouteRefused"
)

var (
	// ErrGatewayRouteUntrusted - a Service carries the gateway route label but
	// is not one the Gateway controls.
	ErrGatewayRouteUntrusted = errors.New("not a gateway route this controller made")
	// ErrGatewayRouteInvalid - a gateway route's annotations do not make a
	// rule.
	ErrGatewayRouteInvalid = errors.New("invalid gateway route")
	// ErrGatewayRouteOnPlainLoxilb - a gateway route would reach a peer that
	// is plain loxilb.
	ErrGatewayRouteOnPlainLoxilb = errors.New("gateway routes are served by loxilb-inference-gateway only")
	// errGatewaysNotSynced - the Gateways are watched but not in the cache
	// yet. Not the Service's fault: it is retried, and not reported.
	errGatewaysNotSynced = errors.New("gateways are not in the cache yet")
)

// gatewayWatch - the Gateways a gateway route is checked against.
type gatewayWatch struct {
	lister gwlister.GatewayLister
	synced func() bool
}

// gatewayRoute - the rule a gateway route Service carries.
type gatewayRoute struct {
	gateway         string
	host            string
	path            string
	match           api.PathMatchModeType
	tls             bool
	backendProtocol api.BackendProtocolType

	backendNamespace string
	backendName      string
	backendPort      int32
}

// routeValue - the route a Service carries, or the zero route for one that
// carries none.
func routeValue(route *gatewayRoute) gatewayRoute {
	if route == nil {
		return gatewayRoute{}
	}
	return *route
}

// key - what tells the rule apart from the others on its listener. The match
// mode is left out: the gateway keeps one endpoint pool for a host and a
// path, and an Exact and a Prefix rule on the same pair would share it.
func (r *gatewayRoute) key() string {
	return r.host + "|" + r.path
}

// SetGatewayLister - the Gateways a gateway route is checked against, and
// whether they are in the cache yet. Without it, as with the Gateway API off,
// no Service is trusted as one.
func (m *Manager) SetGatewayLister(lister gwlister.GatewayLister, synced func() bool) {
	m.gatewayLister.Store(gatewayWatch{lister: lister, synced: synced})
}

// gatewayRouteOf - the route a Service carries: nil and no error for one that
// is not labelled as a gateway route.
func (m *Manager) gatewayRouteOf(svc *corev1.Service) (*gatewayRoute, error) {
	if _, labelled := svc.Labels[gatewayRouteLabel]; !labelled {
		return nil, nil
	}
	gateway, err := m.controllingGateway(svc)
	if err != nil {
		return nil, err
	}
	return parseGatewayRoute(svc, gateway)
}

// gatewayControlled - whether a Gateway controls svc, as controllingGateway
// finds; its gateway routes and its ingress service both are.
func (m *Manager) gatewayControlled(svc *corev1.Service) bool {
	if owner := metav1.GetControllerOf(svc); owner == nil || owner.Kind != "Gateway" {
		return false
	}
	_, err := m.controllingGateway(svc)
	return err == nil
}

// trustedGatewayRoute - the route of a Service, or nil for one that carries
// none or is not trusted with it.
func (m *Manager) trustedGatewayRoute(svc *corev1.Service) *gatewayRoute {
	route, err := m.gatewayRouteOf(svc)
	if err != nil {
		return nil
	}
	return route
}

// controllingGateway - the Gateway that controls svc, by name: the controller
// owner reference names a Gateway of the Service's namespace with its UID,
// and the Service asks for one of its addresses.
func (m *Manager) controllingGateway(svc *corev1.Service) (string, error) {
	owner := metav1.GetControllerOf(svc)
	if owner == nil || owner.Kind != "Gateway" || !strings.HasPrefix(owner.APIVersion, gwv1.GroupName+"/") {
		return "", fmt.Errorf("service %s/%s is not controlled by a Gateway: %w", svc.Namespace, svc.Name, ErrGatewayRouteUntrusted)
	}
	gateway := owner.Name

	watch, _ := m.gatewayLister.Load().(gatewayWatch)
	if watch.lister == nil {
		return "", fmt.Errorf("service %s/%s: Gateways are not watched: %w", svc.Namespace, svc.Name, ErrGatewayRouteUntrusted)
	}
	if watch.synced != nil && !watch.synced() {
		return "", fmt.Errorf("service %s/%s: %w", svc.Namespace, svc.Name, errGatewaysNotSynced)
	}
	gw, err := watch.lister.Gateways(svc.Namespace).Get(gateway)
	if err != nil {
		return "", fmt.Errorf("service %s/%s: gateway %s: %w", svc.Namespace, svc.Name, gateway, err)
	}
	if gw.UID != owner.UID {
		return "", fmt.Errorf("service %s/%s names Gateway %s, but not the one there now: %w", svc.Namespace, svc.Name, gateway, ErrGatewayRouteUntrusted)
	}
	if svc.Spec.LoadBalancerIP == "" || !slices.ContainsFunc(gw.Spec.Addresses, func(a gwv1.GatewaySpecAddress) bool {
		return a.Value == svc.Spec.LoadBalancerIP
	}) {
		return "", fmt.Errorf("service %s/%s: loadBalancerIP %q is not an address of Gateway %s: %w",
			svc.Namespace, svc.Name, svc.Spec.LoadBalancerIP, gateway, ErrGatewayRouteUntrusted)
	}
	return gateway, nil
}

// parseGatewayRoute - the rule a trusted gateway route's annotations make.
func parseGatewayRoute(svc *corev1.Service, gateway string) (*gatewayRoute, error) {
	invalid := func(format string, args ...interface{}) error {
		return fmt.Errorf("service %s/%s: %s: %w", svc.Namespace, svc.Name, fmt.Sprintf(format, args...), ErrGatewayRouteInvalid)
	}
	a := svc.Annotations
	route := &gatewayRoute{gateway: gateway, host: a[gatewayRouteHostAnnotation], path: a[gatewayRoutePathAnnotation]}

	if !strings.HasPrefix(route.path, "/") {
		return nil, invalid("%s %q does not start with /", gatewayRoutePathAnnotation, route.path)
	}

	switch a[gatewayRoutePathMatchAnnotation] {
	case "", string(api.PathMatchModePrefix):
		route.match = api.PathMatchModePrefix
	case string(api.PathMatchModeExact):
		route.match = api.PathMatchModeExact
	default:
		return nil, invalid("%s %q is not prefix or exact", gatewayRoutePathMatchAnnotation, a[gatewayRoutePathMatchAnnotation])
	}

	route.tls = a[gatewayRouteTLSAnnotation] == "true"

	switch a[gatewayRouteBackendProtocolAnnotation] {
	case "", string(api.BackendProtocolHTTP1):
		route.backendProtocol = api.BackendProtocolHTTP1
	case string(api.BackendProtocolHTTP2):
		route.backendProtocol = api.BackendProtocolHTTP2
	default:
		return nil, invalid("%s %q is not http1 or http2", gatewayRouteBackendProtocolAnnotation, a[gatewayRouteBackendProtocolAnnotation])
	}

	namespace, name, ok := strings.Cut(a[gatewayRouteBackendAnnotation], "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return nil, invalid("%s %q is not namespace/name", gatewayRouteBackendAnnotation, a[gatewayRouteBackendAnnotation])
	}
	route.backendNamespace, route.backendName = namespace, name

	port, err := strconv.Atoi(a[gatewayRouteBackendPortAnnotation])
	if err != nil || port < 1 || port > 65535 {
		return nil, invalid("%s %q is not a port", gatewayRouteBackendPortAnnotation, a[gatewayRouteBackendPortAnnotation])
	}
	route.backendPort = int32(port)
	return route, nil
}

// gatewayRouteBackendIndex - the Service cache index of gateway routes by the
// backend whose endpoints they read, namespace/name.
const gatewayRouteBackendIndex = "gatewayRouteBackend"

func gatewayRouteBackendOf(obj interface{}) ([]string, error) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil, nil
	}
	if _, labelled := svc.Labels[gatewayRouteLabel]; !labelled {
		return nil, nil
	}
	if backend := svc.Annotations[gatewayRouteBackendAnnotation]; backend != "" {
		return []string{backend}, nil
	}
	return nil, nil
}

// watchGatewayRouteBackends - have a change to a backend's EndpointSlices
// queue the gateway routes that read them. A route's own Service has no
// endpoints, so nothing else would tell it its backend's pods changed until
// the next resync.
func (m *Manager) watchGatewayRouteBackends(services, endpointSlices cache.SharedIndexInformer) {
	if err := services.AddIndexers(cache.Indexers{gatewayRouteBackendIndex: gatewayRouteBackendOf}); err != nil {
		klog.Errorf("gateway routes: unable to index Services by backend; their endpoints follow the resync only: %v", err)
		return
	}
	m.serviceIndexer = services.GetIndexer()

	_, err := endpointSlices.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.enqueueGatewayRoutesOfSlice,
		UpdateFunc: func(old, cur interface{}) { m.enqueueGatewayRoutesOfSlice(cur) },
		DeleteFunc: m.enqueueGatewayRoutesOfSlice,
	})
	if err != nil {
		klog.Errorf("gateway routes: unable to watch EndpointSlices; their endpoints follow the resync only: %v", err)
	}
}

// enqueueGatewayRoutesOfSlice - queue the gateway routes whose backend an
// EndpointSlice belongs to.
func (m *Manager) enqueueGatewayRoutesOfSlice(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	eps, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok || m.serviceIndexer == nil {
		return
	}
	service := eps.Labels[discoveryv1.LabelServiceName]
	if service == "" {
		return
	}
	routes, err := m.serviceIndexer.ByIndex(gatewayRouteBackendIndex, eps.Namespace+"/"+service)
	if err != nil {
		klog.Errorf("gateway routes of %s/%s: %v", eps.Namespace, service, err)
		return
	}
	for _, route := range routes {
		m.enqueueService(route)
	}
}

// routeEndpoints - the ready addresses of a route's backend Service, and the
// one port its pods serve the route's backend port on.
//
// Read from the backend's EndpointSlices, not by its selector: a backend may
// have none, its endpoints written by another controller - Knative's public
// Services are - and it may be in another namespace than the route's Service.
func (m *Manager) routeEndpoints(route *gatewayRoute, addrType string) ([]string, []int, error) {
	if m.serviceLister == nil || m.endpointSliceLister == nil {
		return nil, nil, fmt.Errorf("backend %s/%s: Services and EndpointSlices are not watched", route.backendNamespace, route.backendName)
	}
	backend, err := m.serviceLister.Services(route.backendNamespace).Get(route.backendName)
	if err != nil {
		return nil, nil, fmt.Errorf("backend %s/%s: %w", route.backendNamespace, route.backendName, err)
	}

	portName, found := "", false
	for _, p := range backend.Spec.Ports {
		if p.Port == route.backendPort {
			portName, found = p.Name, true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("backend %s/%s has no port %d", route.backendNamespace, route.backendName, route.backendPort)
	}

	wantType := discoveryv1.AddressTypeIPv4
	if addrType == "ipv6" {
		wantType = discoveryv1.AddressTypeIPv6
	}
	endpointSlices, err := m.endpointSliceLister.EndpointSlices(route.backendNamespace).List(
		labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: route.backendName}))
	if err != nil {
		return nil, nil, fmt.Errorf("backend %s/%s: %w", route.backendNamespace, route.backendName, err)
	}

	var ips []string
	ipSeen := map[string]bool{}
	var targetPorts []int
	for _, eps := range endpointSlices {
		if eps.AddressType != wantType {
			continue
		}
		var target *int32
		for _, p := range eps.Ports {
			name := ""
			if p.Name != nil {
				name = *p.Name
			}
			if name == portName && p.Port != nil {
				target = p.Port
				break
			}
		}
		if target == nil {
			continue
		}

		ready := false
		for _, ep := range eps.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				ready = true
				if !ipSeen[addr] {
					ipSeen[addr] = true
					ips = append(ips, addr)
				}
			}
		}
		if ready && !slices.Contains(targetPorts, int(*target)) {
			targetPorts = append(targetPorts, int(*target))
		}
	}

	// Each endpoint goes to loxilb with every target port, so pods that serve
	// the port on different numbers - a named target port that differs
	// between them - cannot be told apart in one rule.
	if len(targetPorts) > 1 {
		return nil, nil, fmt.Errorf("backend %s/%s serves port %d on more than one target port %v; one rule cannot route each pod to its own",
			route.backendNamespace, route.backendName, route.backendPort, targetPorts)
	}
	slices.Sort(ips)
	return ips, targetPorts, nil
}

// GatewayPeers - whether every loxilb peer is loxilb-inference-gateway, which
// serving a route as a gateway route needs, and why not when one is not. A
// peer that has not reported its flavor yet counts as not one; when it does,
// SetPeerBackHook's hooks are called.
func (m *Manager) GatewayPeers() (bool, string) {
	if m.LoxiClients == nil || len(m.LoxiClients.Clients) == 0 {
		return false, "there is no loxilb peer"
	}
	for _, c := range m.LoxiClients.Clients {
		if !c.FlavorDetected() {
			return false, fmt.Sprintf("loxilb-lb(%s) has not reported what it is yet", c.Host)
		}
		if !c.IsInferenceGateway() {
			return false, fmt.Sprintf("loxilb-lb(%s) is plain loxilb", c.Host)
		}
	}
	return true, ""
}
