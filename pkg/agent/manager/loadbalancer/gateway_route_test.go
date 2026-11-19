package loadbalancer

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwlister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func routeIndexer(objs ...interface{}) cache.Indexer {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, obj := range objs {
		_ = indexer.Add(obj)
	}
	return indexer
}

// routeGateway - Gateway infra/gw on 10.0.0.10.
func routeGateway() *gwv1.Gateway {
	return &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw", UID: types.UID("gw-uid")},
		Spec:       gwv1.GatewaySpec{Addresses: []gwv1.GatewaySpecAddress{{Value: "10.0.0.10"}}},
	}
}

// routeService - the gateway route for shop.example.com/api on gw's port 80,
// to Service apps/web port 80.
func routeService(mutate ...func(*corev1.Service)) *corev1.Service {
	class := addTestClass
	controller := true
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "infra", Name: "gw-route-1",
			Labels: map[string]string{gatewayRouteLabel: "true"},
			Annotations: map[string]string{
				lbModeAnnotation:                  "fullproxy",
				gatewayRouteHostAnnotation:        "shop.example.com",
				gatewayRoutePathAnnotation:        "/api",
				gatewayRoutePathMatchAnnotation:   "prefix",
				gatewayRouteBackendAnnotation:     "apps/web",
				gatewayRouteBackendPortAnnotation: "80",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway", Name: "gw", UID: types.UID("gw-uid"), Controller: &controller,
			}},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &class, LoadBalancerIP: "10.0.0.10",
			Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	for _, f := range mutate {
		f(svc)
	}
	return svc
}

func backendService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "metrics", Port: 9090, TargetPort: intstr.FromInt(9090)},
			{Name: "http", Port: 80, TargetPort: intstr.FromString("http")},
		}},
	}
}

func endpointSlice(name string, addrType discoveryv1.AddressType, portName string, port int32, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	pn := portName
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "apps", Name: name, Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: addrType,
		Ports:       []discoveryv1.EndpointPort{{Name: &pn, Port: &port}},
		Endpoints:   endpoints,
	}
}

func endpointAt(ready bool, addrs ...string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{Addresses: addrs, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}
}

// routeManager - a manager that knows gw, apps/web and its endpoints.
func routeManager(objs ...interface{}) *Manager {
	var services, slices []interface{}
	for _, obj := range objs {
		switch obj.(type) {
		case *corev1.Service:
			services = append(services, obj)
		case *discoveryv1.EndpointSlice:
			slices = append(slices, obj)
		}
	}
	m := &Manager{
		networkConfig:       &config.NetworkConfig{},
		lbCache:             LbCacheTable{},
		serviceLister:       corelisters.NewServiceLister(routeIndexer(services...)),
		endpointSliceLister: discoverylisters.NewEndpointSliceLister(routeIndexer(slices...)),
	}
	m.SetGatewayLister(gwlister.NewGatewayLister(routeIndexer(routeGateway())), func() bool { return true })
	return m
}

// A Service gets a gateway route's share of a listener and reach into
// another namespace's endpoints only when the Gateway controls it. One that
// merely carries the label and annotations is refused.
func TestGatewayRouteTrust(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*corev1.Service)
		lister  bool
		wantErr error
	}{
		{name: "controlled by the Gateway", lister: true},
		{name: "no owner", lister: true, wantErr: ErrGatewayRouteUntrusted,
			mutate: func(s *corev1.Service) { s.OwnerReferences = nil }},
		{name: "owned but not controlled", lister: true, wantErr: ErrGatewayRouteUntrusted,
			mutate: func(s *corev1.Service) { s.OwnerReferences[0].Controller = nil }},
		{name: "another kind", lister: true, wantErr: ErrGatewayRouteUntrusted,
			mutate: func(s *corev1.Service) { s.OwnerReferences[0].Kind = "HTTPRoute" }},
		{name: "a Gateway of the name, not the one there", lister: true, wantErr: ErrGatewayRouteUntrusted,
			mutate: func(s *corev1.Service) { s.OwnerReferences[0].UID = "other-uid" }},
		// A Service in another namespace naming a Gateway it cannot be owned
		// by: there is no such Gateway in its namespace.
		{name: "another namespace", lister: true,
			mutate: func(s *corev1.Service) { s.Namespace = "apps" }},
		{name: "an address the Gateway does not have", lister: true, wantErr: ErrGatewayRouteUntrusted,
			mutate: func(s *corev1.Service) { s.Spec.LoadBalancerIP = "10.0.0.99" }},
		{name: "Gateways not watched", wantErr: ErrGatewayRouteUntrusted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := routeManager()
			if !tc.lister {
				m = &Manager{networkConfig: &config.NetworkConfig{}}
			}
			svc := routeService()
			if tc.mutate != nil {
				tc.mutate(svc)
			}

			route, err := m.gatewayRouteOf(svc)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("gatewayRouteOf = %v, want %v", err, tc.wantErr)
				}
			case tc.name == "another namespace":
				if err == nil {
					t.Fatal("trusted a Service in another namespace than its Gateway")
				}
			case err != nil:
				t.Fatalf("gatewayRouteOf: %v", err)
			case route == nil || route.key() != "shop.example.com|/api" || route.backendNamespace != "apps" || route.backendPort != 80:
				t.Errorf("route = %+v", route)
			}

			if err != nil && m.listenerAddrOf(svc) != "" {
				t.Error("an untrusted Service was given a share of the Gateway listener")
			}
		})
	}

	if route, err := routeManager().gatewayRouteOf(backendService()); route != nil || err != nil {
		t.Errorf("a Service without the label read as a gateway route: %v, %v", route, err)
	}
}

func TestParseGatewayRoute(t *testing.T) {
	cases := []struct {
		name      string
		key, val  string
		wantMatch api.PathMatchModeType
		wantProto api.BackendProtocolType
		wantErr   bool
	}{
		{name: "defaults", key: gatewayRoutePathMatchAnnotation, val: "", wantMatch: api.PathMatchModePrefix, wantProto: api.BackendProtocolHTTP1},
		{name: "exact", key: gatewayRoutePathMatchAnnotation, val: "exact", wantMatch: api.PathMatchModeExact, wantProto: api.BackendProtocolHTTP1},
		{name: "h2c backend", key: gatewayRouteBackendProtocolAnnotation, val: "http2", wantMatch: api.PathMatchModePrefix, wantProto: api.BackendProtocolHTTP2},
		{name: "a path without its slash", key: gatewayRoutePathAnnotation, val: "api", wantErr: true},
		{name: "an unknown match", key: gatewayRoutePathMatchAnnotation, val: "regex", wantErr: true},
		{name: "an unknown backend protocol", key: gatewayRouteBackendProtocolAnnotation, val: "grpc", wantErr: true},
		{name: "a backend without its namespace", key: gatewayRouteBackendAnnotation, val: "web", wantErr: true},
		{name: "a port out of range", key: gatewayRouteBackendPortAnnotation, val: "70000", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := routeService(func(s *corev1.Service) { s.Annotations[tc.key] = tc.val })
			route, err := parseGatewayRoute(svc, "gw")
			if tc.wantErr {
				if !errors.Is(err, ErrGatewayRouteInvalid) {
					t.Fatalf("parseGatewayRoute = %v, want invalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGatewayRoute: %v", err)
			}
			if route.match != tc.wantMatch || route.backendProtocol != tc.wantProto {
				t.Errorf("route = %+v", route)
			}
		})
	}
}

// The endpoints come from the backend's EndpointSlices, through the port the
// route names: a backend may have no selector of its own.
func TestRouteEndpoints(t *testing.T) {
	route := &gatewayRoute{backendNamespace: "apps", backendName: "web", backendPort: 80}

	t.Run("ready endpoints on the named port", func(t *testing.T) {
		m := routeManager(backendService(),
			endpointSlice("web-a", discoveryv1.AddressTypeIPv4, "http", 8080, endpointAt(true, "10.1.0.2"), endpointAt(false, "10.1.0.9")),
			endpointSlice("web-b", discoveryv1.AddressTypeIPv4, "http", 8080, endpointAt(true, "10.1.0.1")),
			endpointSlice("web-v6", discoveryv1.AddressTypeIPv6, "http", 8080, endpointAt(true, "fd00::1")),
			endpointSlice("web-metrics", discoveryv1.AddressTypeIPv4, "metrics", 9090, endpointAt(true, "10.1.0.7")),
		)
		ips, ports, err := m.routeEndpoints(route, "ipv4")
		if err != nil {
			t.Fatalf("routeEndpoints: %v", err)
		}
		if strings.Join(ips, ",") != "10.1.0.1,10.1.0.2" || len(ports) != 1 || ports[0] != 8080 {
			t.Errorf("endpoints = %v on %v, want the two ready IPv4 ones on 8080", ips, ports)
		}
	})

	t.Run("pods that serve the port on different numbers", func(t *testing.T) {
		m := routeManager(backendService(),
			endpointSlice("web-a", discoveryv1.AddressTypeIPv4, "http", 8080, endpointAt(true, "10.1.0.1")),
			endpointSlice("web-b", discoveryv1.AddressTypeIPv4, "http", 8081, endpointAt(true, "10.1.0.2")),
		)
		if _, _, err := m.routeEndpoints(route, "ipv4"); err == nil {
			t.Error("routeEndpoints accepted two target ports for one rule")
		}
	})

	t.Run("a port the backend does not have", func(t *testing.T) {
		m := routeManager(backendService())
		if _, _, err := m.routeEndpoints(&gatewayRoute{backendNamespace: "apps", backendName: "web", backendPort: 443}, "ipv4"); err == nil {
			t.Error("routeEndpoints accepted a port the backend does not have")
		}
	})

	t.Run("a backend that does not exist", func(t *testing.T) {
		if _, _, err := routeManager().routeEndpoints(route, "ipv4"); err == nil {
			t.Error("routeEndpoints accepted a missing backend")
		}
	})
}

// Two gateway routes on one listener with the same host and path make one
// rule on the gateway, whatever their match modes; other paths do not
// conflict, nor does a Service that only looks like a route.
func TestGatewayRouteConflicts(t *testing.T) {
	m := routeManager()
	a := routeService()
	exact := routeService(func(s *corev1.Service) {
		s.Name = "gw-route-2"
		s.Annotations[gatewayRoutePathMatchAnnotation] = "exact"
	})
	other := routeService(func(s *corev1.Service) {
		s.Name = "gw-route-3"
		s.Annotations[gatewayRoutePathAnnotation] = "/cart"
	})
	untrusted := routeService(func(s *corev1.Service) {
		s.Name = "lookalike"
		s.OwnerReferences = nil
	})

	if got := m.conflictOf(a, exact); got != ReasonListenerRouteConflict {
		t.Errorf("Exact and Prefix on one host and path: conflict %q, want a route conflict", got)
	}
	if got := m.conflictOf(a, other); got != "" {
		t.Errorf("two paths: conflict %q, want none", got)
	}
	if got := m.conflictOf(a, untrusted); got != "" {
		t.Errorf("a lookalike: conflict %q, want none - it must not make the route yield", got)
	}
}

// The rule a gateway route makes: the route's host, path and match on the
// Gateway listener, the backend's pods on their target port.
func TestGatewayRouteModel(t *testing.T) {
	m := routeManager()
	svc := routeService(func(s *corev1.Service) {
		s.Annotations[gatewayRouteTLSAnnotation] = "true"
		s.Annotations[gatewayRouteBackendProtocolAnnotation] = "http2"
	})
	route, err := m.gatewayRouteOf(svc)
	if err != nil {
		t.Fatalf("gatewayRouteOf: %v", err)
	}

	lbArgs := &LbArgs{
		externalIP: "10.0.0.10", lbMode: int(api.LBModeFullProxy), usePodNetwork: true,
		endpointIPs: []string{"10.1.0.1", "10.1.0.2"}, route: route, routeTargetPorts: []int{8080},
	}
	model, err := m.makeLoxiLoadBalancerModel(lbArgs, svc, svc.Spec.Ports[0])
	if err != nil {
		t.Fatalf("makeLoxiLoadBalancerModel: %v", err)
	}

	s := model.Service
	if s.Host != "shop.example.com" || s.PathPrefix != "/api" || s.PathMatchMode != api.PathMatchModePrefix ||
		s.Security != 1 || s.BackendProtocol != api.BackendProtocolHTTP2 || !s.GatewayRoute ||
		s.ExternalIP != "10.0.0.10" || s.Port != 80 || s.Mode != api.LBModeFullProxy {
		t.Errorf("service = %+v", s)
	}
	if len(model.Endpoints) != 2 || model.Endpoints[0].TargetPort != 8080 || model.Endpoints[1].TargetPort != 8080 {
		t.Errorf("endpoints = %+v, want both pods on 8080", model.Endpoints)
	}
}

// A gateway route reaches loxilb-inference-gateway peers only. A plain peer
// would take it, drop what it does not know and serve it with its own
// certificate.
func TestInstallLBRefusesGatewayRouteOnPlainLoxilb(t *testing.T) {
	plain := newFakeLoxiLB(t, "")
	gateway := newFakeLoxiLB(t, api.ProductInferenceGateway)
	model := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.10", Port: 80, Protocol: "tcp", Mode: api.LBModeFullProxy,
			Host: "shop.example.com", PathPrefix: "/api", PathMatchMode: api.PathMatchModePrefix, GatewayRoute: true,
		},
		Endpoints: []api.LoadBalancerEndpoint{{EndpointIP: "10.1.0.1", TargetPort: 8080, Weight: 1}},
	}

	m := &Manager{}
	err := m.installLB(plain.client(t), model, false)
	if !errors.Is(err, ErrGatewayRouteOnPlainLoxilb) {
		t.Fatalf("installLB on plain loxilb = %v, want it refused", err)
	}
	plain.mu.Lock()
	sent := len(plain.bodies)
	plain.mu.Unlock()
	if sent != 0 {
		t.Errorf("the rule still reached plain loxilb: %d requests", sent)
	}

	if err := m.installLB(gateway.client(t), model, false); err != nil {
		t.Fatalf("installLB on the gateway: %v", err)
	}
	if body := gateway.lastBody(t); strings.Contains(body, "GatewayRoute") {
		t.Errorf("the marker was sent: %s", body)
	}
}

// Before the Gateways are in the cache, a gateway route is neither trusted nor
// refused: it waits, and no warning is raised for what is only startup.
func TestGatewayRouteWaitsForTheGateways(t *testing.T) {
	m := routeManager()
	m.SetGatewayLister(gwlister.NewGatewayLister(routeIndexer()), func() bool { return false })

	_, err := m.gatewayRouteOf(routeService())
	if !errors.Is(err, errGatewaysNotSynced) || errors.Is(err, ErrGatewayRouteUntrusted) {
		t.Errorf("gatewayRouteOf before the sync = %v, want it told to wait", err)
	}
}

// A change to the backend's EndpointSlices reaches the gateway routes that read
// them, and only those: the route's own Service has no endpoints to notice
// it by.
func TestEndpointSliceQueuesItsGatewayRoutes(t *testing.T) {
	factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	services := factory.Core().V1().Services().Informer()
	slices := factory.Discovery().V1().EndpointSlices().Informer()

	m := &Manager{queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())}
	m.watchGatewayRouteBackends(services, slices)

	other := routeService(func(s *corev1.Service) {
		s.Name = "gw-route-2"
		s.Annotations[gatewayRouteBackendAnnotation] = "apps/cart"
	})
	for _, svc := range []*corev1.Service{routeService(), other, backendService()} {
		if err := services.GetIndexer().Add(svc); err != nil {
			t.Fatal(err)
		}
	}

	m.enqueueGatewayRoutesOfSlice(cache.DeletedFinalStateUnknown{
		Obj: endpointSlice("web-a", discoveryv1.AddressTypeIPv4, "http", 8080, endpointAt(true, "10.1.0.1")),
	})
	if m.queue.Len() != 1 {
		t.Fatalf("queued %d Services, want the one route that reads apps/web", m.queue.Len())
	}
	item, _ := m.queue.Get()
	if item != (LbCacheKey{Namespace: "infra", Name: "gw-route-1"}) {
		t.Errorf("queued %v, want infra/gw-route-1", item)
	}
}
