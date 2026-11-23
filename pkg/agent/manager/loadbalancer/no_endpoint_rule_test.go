package loadbalancer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/util/workqueue"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwlister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// gatewayPeer - a fake gateway that counts endpoints as the gateway does,
// listing the features given.
func gatewayPeer(t *testing.T, features ...string) *fakeLoxiLB {
	f := newFakeLoxiLB(t, api.ProductInferenceGateway)
	f.features = features
	f.countsEndpoints = true
	return f
}

// routeTo - a manager serving the gateway route infra/gw-route-1 on
// 192.168.0.13 to peers, and the route. objs are the backends and
// EndpointSlices it knows besides the route.
func routeTo(t *testing.T, peers []*fakeLoxiLB, mutate func(*corev1.Service), objs ...interface{}) (*Manager, *corev1.Service) {
	t.Helper()
	var clients []*api.LoxiClient
	for _, p := range peers {
		c := p.client(t)
		c.IsAlive = true
		clients = append(clients, c)
	}
	m, _ := addHarness(t, clients...)

	gateway := routeGateway()
	gateway.Spec.Addresses = []gwv1.GatewaySpecAddress{{Value: "192.168.0.13"}}
	m.SetGatewayLister(gwlister.NewGatewayLister(routeIndexer(gateway)), func() bool { return true })
	route := routeService(func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.168.0.13" })
	if mutate != nil {
		mutate(route)
	}
	if _, err := m.kubeClient.CoreV1().Services(route.Namespace).Create(context.Background(), route, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create route Service: %v", err)
	}
	m.knows(route, objs...)
	return m, route
}

// knows - the Services and EndpointSlices the manager reads from now on.
func (m *Manager) knows(route *corev1.Service, objs ...interface{}) {
	services, slices := []interface{}{route}, []interface{}{}
	for _, obj := range objs {
		switch obj.(type) {
		case *corev1.Service:
			services = append(services, obj)
		case *discoveryv1.EndpointSlice:
			slices = append(slices, obj)
		}
	}
	m.serviceLister = corelisters.NewServiceLister(routeIndexer(services...))
	m.endpointSliceLister = discoverylisters.NewEndpointSliceLister(routeIndexer(slices...))
}

// webReady, webUnready - apps/web's one pod, ready or not.
func webReady() *discoveryv1.EndpointSlice {
	return endpointSlice("web-1", discoveryv1.AddressTypeIPv4, "http", 8000, endpointAt(true, "10.1.0.1"))
}

func webUnready() *discoveryv1.EndpointSlice {
	return endpointSlice("web-1", discoveryv1.AddressTypeIPv4, "http", 8000, endpointAt(false, "10.1.0.1"))
}

// sentRule - what the last POST to f asked for: its endpoints, and the
// status it answers without them.
type sentRule struct {
	Endpoints []struct {
		EndpointIP string `json:"endpointIP"`
		TargetPort int    `json:"targetPort"`
	} `json:"endpoints"`
	Service struct {
		NoEndpointStatus *int32 `json:"no_endpoint_status"`
	} `json:"serviceArguments"`
	raw string
}

func lastRule(t *testing.T, f *fakeLoxiLB) sentRule {
	t.Helper()
	body := f.lastBody(t)
	var r sentRule
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("last rule sent: %v", err)
	}
	r.raw = body
	return r
}

func (r sentRule) status() int32 {
	if r.Service.NoEndpointStatus == nil {
		return 0
	}
	return *r.Service.NoEndpointStatus
}

// installedAt - whether the route's status reports its address, which tells
// the Gateway API manager its rule is on the peers.
func installedAt(t *testing.T, m *Manager, route *corev1.Service) bool {
	t.Helper()
	got, err := m.kubeClient.CoreV1().Services(route.Namespace).Get(context.Background(), route.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get route Service: %v", err)
	}
	return len(got.Status.LoadBalancer.Ingress) > 0
}

// A route whose backend has no ready endpoint is a rule without endpoints
// that answers 503, where every peer takes one. The requests it matches are
// answered there, not by the longest other prefix of its host.
func TestARouteWithoutEndpointsIsARuleThatAnswers503(t *testing.T) {
	peers := []*fakeLoxiLB{gatewayPeer(t, api.FeatureNoEndpointRule), gatewayPeer(t, api.FeatureNoEndpointRule)}
	m, route := routeTo(t, peers, nil, backendService(), webUnready())

	if err := m.addLoadBalancer(route); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	for i, p := range peers {
		sent := lastRule(t, p)
		if len(sent.Endpoints) != 0 || !strings.Contains(sent.raw, `"endpoints":[]`) {
			t.Errorf("peer %d: endpoints sent = %s, want []", i, sent.raw)
		}
		if sent.status() != 503 {
			t.Errorf("peer %d: no_endpoint_status = %d, want 503", i, sent.status())
		}
	}
	if !installedAt(t, m, route) {
		t.Error("the route does not report its address, so its port would not leave the ingress service")
	}
}

// Where a peer does not take such a rule, the route is not sent, as before:
// the peers answer its requests alike.
func TestARouteWithoutEndpointsIsNotSentWhereAPeerDoesNotTakeIt(t *testing.T) {
	for name, second := range map[string]*fakeLoxiLB{
		"without the feature": gatewayPeer(t),
		"plain loxilb":        newFakeLoxiLB(t, ""),
	} {
		t.Run(name, func(t *testing.T) {
			peers := []*fakeLoxiLB{gatewayPeer(t, api.FeatureNoEndpointRule), second}
			m, route := routeTo(t, peers, nil, backendService(), webUnready())

			if err := m.addLoadBalancer(route); err == nil || !strings.Contains(err.Error(), "no active endpoints") {
				t.Fatalf("addLoadBalancer = %v, want the route left out for having no endpoints", err)
			}
			for i, p := range peers {
				if n := posts(p); n != 0 {
					t.Errorf("peer %d got %d rules", i, n)
				}
			}
		})
	}
}

// A backend the spec calls invalid is answered 500: a Service that does not
// exist, and one the Gateway API manager found invalid - not a Service, or
// not one the route may reach.
func TestAnInvalidBackendIsARuleThatAnswers500(t *testing.T) {
	invalid := func(s *corev1.Service) {
		delete(s.Annotations, gatewayRouteBackendAnnotation)
		delete(s.Annotations, gatewayRouteBackendPortAnnotation)
		s.Annotations[gatewayRouteInvalidBackendAnnotation] = "no ReferenceGrant in namespace apps permits Service web"
	}
	for name, tc := range map[string]struct {
		mutate func(*corev1.Service)
		objs   []interface{}
	}{
		"no such Service": {},
		"invalid":         {mutate: invalid, objs: []interface{}{backendService(), webReady()}},
	} {
		t.Run(name, func(t *testing.T) {
			p := gatewayPeer(t, api.FeatureNoEndpointRule)
			m, route := routeTo(t, []*fakeLoxiLB{p}, tc.mutate, tc.objs...)

			if err := m.addLoadBalancer(route); err != nil {
				t.Fatalf("addLoadBalancer: %v", err)
			}
			if sent := lastRule(t, p); len(sent.Endpoints) != 0 || sent.status() != 500 {
				t.Errorf("rule sent = %s, want no endpoints and no_endpoint_status 500", sent.raw)
			}

			// Without the feature, the route is not sent, as before.
			plain := gatewayPeer(t)
			m, route = routeTo(t, []*fakeLoxiLB{plain}, tc.mutate, tc.objs...)
			if err := m.addLoadBalancer(route); err == nil {
				t.Error("addLoadBalancer without the feature succeeded, want the route left out")
			}
			if n := posts(plain); n != 0 {
				t.Errorf("the peer without the feature got %d rules", n)
			}
		})
	}
}

// The rule stays while its endpoints go and come back: the gateway changes
// them in place, so its requests never reach another rule in between. A rule
// made without endpoints takes its first ones in place too.
func TestARuleStaysWhileItsEndpointsGoAndComeBack(t *testing.T) {
	for name, first := range map[string]*discoveryv1.EndpointSlice{
		"made with endpoints":    webReady(),
		"made without endpoints": webUnready(),
	} {
		t.Run(name, func(t *testing.T) {
			p := gatewayPeer(t, api.FeatureNoEndpointRule)
			m, route := routeTo(t, []*fakeLoxiLB{p}, nil, backendService(), first)
			if err := m.addLoadBalancer(route); err != nil {
				t.Fatalf("addLoadBalancer: %v", err)
			}
			_, deletes, _ := p.counts()

			for _, step := range []struct {
				slice  *discoveryv1.EndpointSlice
				eps    int
				status int32
			}{
				{webUnready(), 0, 503},
				{webReady(), 1, 0},
				{webUnready(), 0, 503},
			} {
				m.knows(route, backendService(), step.slice)
				if err := m.addLoadBalancer(route); err != nil {
					t.Fatalf("addLoadBalancer: %v", err)
				}
				sent := lastRule(t, p)
				if len(sent.Endpoints) != step.eps || sent.status() != step.status {
					t.Errorf("rule sent = %s, want %d endpoints and no_endpoint_status %d", sent.raw, step.eps, step.status)
				}
				if step.eps > 0 && sent.Endpoints[0].TargetPort != 8000 {
					t.Errorf("endpoint sent on %d, want 8000", sent.Endpoints[0].TargetPort)
				}
			}
			if _, after, _ := p.counts(); after != deletes {
				t.Errorf("the rule was deleted %d times while its endpoints changed, want in place", after-deletes)
			}
		})
	}
}

// Where the peers do not take a rule without endpoints, the rule goes when its
// last endpoint does, as before the feature.
func TestARuleGoesWithItsLastEndpointWhereAPeerDoesNotTakeOneWithout(t *testing.T) {
	p := gatewayPeer(t)
	m, route := routeTo(t, []*fakeLoxiLB{p}, nil, backendService(), webReady())
	if err := m.addLoadBalancer(route); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	_, deletes, _ := p.counts()

	m.knows(route, backendService(), webUnready())
	if err := m.addLoadBalancer(route); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	if _, after, _ := p.counts(); after == deletes {
		t.Error("the rule was not deleted with its last endpoint")
	}
	if p.storedRule(t) != nil {
		t.Error("the peer still holds the rule")
	}
}

// Another backend for the same host and path changes the rule's endpoints in
// place; another match mode is another rule, and replaces it.
func TestABackendChangeKeepsTheRule(t *testing.T) {
	p := gatewayPeer(t, api.FeatureNoEndpointRule)
	m, route := routeTo(t, []*fakeLoxiLB{p}, nil, backendService(), webReady())
	if err := m.addLoadBalancer(route); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	_, deletes, _ := p.counts()

	cart := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "cart"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}}},
	}
	cartSlice := endpointSlice("cart-1", discoveryv1.AddressTypeIPv4, "http", 8000, endpointAt(true, "10.1.0.2"))
	cartSlice.Labels[discoveryv1.LabelServiceName] = "cart"
	moved := route.DeepCopy()
	moved.Annotations[gatewayRouteBackendAnnotation] = "apps/cart"
	m.knows(moved, backendService(), webReady(), cart, cartSlice)
	if err := m.addLoadBalancer(moved); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	if sent := lastRule(t, p); len(sent.Endpoints) != 1 || sent.Endpoints[0].EndpointIP != "10.1.0.2" {
		t.Errorf("rule sent = %s, want apps/cart's endpoint", sent.raw)
	}

	// And from an invalid backend to it, which the route's fix does.
	invalid := moved.DeepCopy()
	delete(invalid.Annotations, gatewayRouteBackendAnnotation)
	delete(invalid.Annotations, gatewayRouteBackendPortAnnotation)
	invalid.Annotations[gatewayRouteInvalidBackendAnnotation] = "Service apps/cart is not permitted"
	for _, svc := range []*corev1.Service{invalid, moved} {
		m.knows(svc, backendService(), webReady(), cart, cartSlice)
		if err := m.addLoadBalancer(svc); err != nil {
			t.Fatalf("addLoadBalancer: %v", err)
		}
	}
	if _, after, _ := p.counts(); after != deletes {
		t.Errorf("the rule was deleted %d times for a change of backend, want in place", after-deletes)
	}

	exact := moved.DeepCopy()
	exact.Annotations[gatewayRoutePathMatchAnnotation] = "exact"
	m.knows(exact, backendService(), webReady(), cart, cartSlice)
	if err := m.addLoadBalancer(exact); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	if _, after, _ := p.counts(); after == deletes {
		t.Error("the rule with the old match mode was not deleted")
	}
}

// A peer replaced by a build without the feature since the check is not sent
// a rule it would refuse.
func TestInstallLBSendsARuleWithoutEndpointsOnlyWhereItIsTaken(t *testing.T) {
	model := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.10", Port: 80, Protocol: "tcp", Mode: api.LBModeFullProxy,
			Host: "shop.example.com", PathPrefix: "/api", PathMatchMode: api.PathMatchModePrefix,
			GatewayRoute: true, NoEndpointStatus: 503,
		},
		Endpoints: []api.LoadBalancerEndpoint{},
	}
	m := &Manager{}

	without := gatewayPeer(t)
	if err := m.installLB(without.client(t), model, false); !errors.Is(err, ErrNoEndpointRuleUnsupported) {
		t.Errorf("installLB on a peer without the feature = %v, want it declined", err)
	}
	if n := posts(without); n != 0 {
		t.Errorf("the peer without the feature got %d rules", n)
	}

	with := gatewayPeer(t, api.FeatureNoEndpointRule)
	if err := m.installLB(with.client(t), model, false); err != nil {
		t.Errorf("installLB on a peer with the feature: %v", err)
	}
}

// The status is read back with the rule it was sent with, and only then.
func TestReadBackChecksTheStatusOfARuleWithoutEndpoints(t *testing.T) {
	sent := &api.LoadBalancerService{ExternalIP: "10.0.0.10", Port: 80, Protocol: "tcp", NoEndpointStatus: 500}
	got := &api.RuleReadBack{ExternalIP: "10.0.0.10", Port: 80, Protocol: "tcp", NoEndpointStatus: 503}
	if diff := ruleDiff(sent, got); len(diff) != 1 || !strings.Contains(diff[0], "no_endpoint_status") {
		t.Errorf("ruleDiff = %v, want no_endpoint_status to differ", diff)
	}
	sent.NoEndpointStatus = 0
	if diff := ruleDiff(sent, got); len(diff) != 0 {
		t.Errorf("ruleDiff of a rule sent with endpoints = %v, want none", diff)
	}
}

// A backend Service coming or going queues the routes that name it: one that
// does not exist is answered 500, and an EndpointSlice need not follow.
func TestABackendComingOrGoingQueuesItsGatewayRoutes(t *testing.T) {
	factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	services := factory.Core().V1().Services().Informer()
	m := &Manager{queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())}
	m.watchGatewayRouteBackends(services, factory.Discovery().V1().EndpointSlices().Informer())
	if err := services.GetIndexer().Add(routeService()); err != nil {
		t.Fatal(err)
	}

	m.enqueueGatewayRoutesOfBackend(backendService())
	if m.queue.Len() != 1 {
		t.Fatalf("queued %d Services, want the route that names apps/web", m.queue.Len())
	}
	if item, _ := m.queue.Get(); item != (LbCacheKey{Namespace: "infra", Name: "gw-route-1"}) {
		t.Errorf("queued %v, want infra/gw-route-1", item)
	}
}

// Every peer, detected and listing the feature.
func TestServesRulesWithoutEndpoints(t *testing.T) {
	with, without := gatewayPeer(t, api.FeatureNoEndpointRule), gatewayPeer(t)
	undetected := &api.LoxiClient{Host: "10.9.9.9"}
	for name, tc := range map[string]struct {
		peers []*api.LoxiClient
		want  bool
	}{
		"no peer":                 {nil, false},
		"every peer":              {[]*api.LoxiClient{with.client(t), with.client(t)}, true},
		"one without the feature": {[]*api.LoxiClient{with.client(t), without.client(t)}, false},
		"one not detected yet":    {[]*api.LoxiClient{with.client(t), undetected}, false},
	} {
		t.Run(name, func(t *testing.T) {
			pool := api.NewLoxiClientPool()
			for _, c := range tc.peers {
				pool.AddLoxiClient(c)
			}
			m := &Manager{LoxiClients: pool}
			if got := m.ServesRulesWithoutEndpoints(); got != tc.want {
				t.Errorf("ServesRulesWithoutEndpoints = %v, want %v", got, tc.want)
			}
		})
	}
}
