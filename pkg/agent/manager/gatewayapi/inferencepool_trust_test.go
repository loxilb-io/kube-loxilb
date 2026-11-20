package gatewayapi

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1lister "k8s.io/client-go/listers/core/v1"
	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

// otherTeamsGateway - a Gateway in another namespace whose listeners take
// routes as given, at 123.123.123.9.
func otherTeamsGateway(class string, listeners ...v1.Listener) *v1.Gateway {
	return &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "team-gw", Namespace: "team"},
		Spec:       v1.GatewaySpec{GatewayClassName: v1.ObjectName(class), Listeners: listeners},
		Status:     v1.GatewayStatus{Addresses: []v1.GatewayStatusAddress{{Value: "123.123.123.9"}}},
	}
}

func createGateway(t *testing.T, m *InferencePoolManager, gw *v1.Gateway) {
	t.Helper()
	if _, err := m.sigsClient.GatewayV1().Gateways(gw.Namespace).Create(context.Background(), gw, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
}

// A pool's Service takes its Gateway listener's address and port, so the
// route sending it there has to attach to the listener. One that takes routes
// from its own namespace only gives a pool in another nothing - it used to
// give it the listener.
func TestPoolIsNotServedOnAListenerThatRefusesItsRoute(t *testing.T) {
	m := reconcileManager(testPool(nil))
	createGateway(t, m, otherTeamsGateway("loxilb.io/loxilb",
		v1.Listener{Name: "http", Port: 8080, Protocol: v1.HTTPProtocolType}))
	team := v1.Namespace("team")
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
		poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "team-gw", Namespace: &team})))

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if services := ownedServices(t, m); len(services) != 0 {
		t.Fatalf("a Service was made on a listener that does not take the route: %v", serviceNames(services))
	}
	accepted := poolCondition(t, m, string(infv1.InferencePoolConditionAccepted))
	if accepted.Status != metav1.ConditionFalse || accepted.Reason != string(infv1.InferencePoolReasonHTTPRouteNotAccepted) ||
		!strings.Contains(accepted.Message, "takes routes from namespace team only") {
		t.Errorf("Accepted = %s/%s %q, want False/HTTPRouteNotAccepted saying why", accepted.Status, accepted.Reason, accepted.Message)
	}

	// The same Service written by hand is not one this manager made.
	forged := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm-qwen3-inference", Namespace: "llm",
			Labels: map[string]string{poolOwnerLabel: "vllm-qwen3"}},
		Spec: corev1.ServiceSpec{LoadBalancerIP: "123.123.123.9",
			Ports: []corev1.ServicePort{{Name: "inference", Port: 8080, Protocol: corev1.ProtocolTCP}}},
	}
	if generated, decided := m.Generated(forged); generated || !decided {
		t.Errorf("Generated = %v (decided %v) for a Service no listener allows, want false and decided", generated, decided)
	}
}

// A Gateway of another controller's class is that controller's: a route
// sending a pool there gives the pool no Service here, and no parent status
// either.
func TestPoolIsNotServedOnAnotherControllersGateway(t *testing.T) {
	m := reconcileManager(testPool(nil))
	all := v1.NamespacesFromAll
	createGateway(t, m, otherTeamsGateway("other-gc", v1.Listener{Name: "http", Port: 8080, Protocol: v1.HTTPProtocolType,
		AllowedRoutes: fromNamespaces(all, nil)}))
	team := v1.Namespace("team")
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
		poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "team-gw", Namespace: &team})))

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if services := ownedServices(t, m); len(services) != 0 {
		t.Fatalf("a Service was made on another controller's Gateway: %v", serviceNames(services))
	}
	pool, err := m.infClient.InferenceV1().InferencePools("llm").Get(context.Background(), "vllm-qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if len(pool.Status.Parents) != 0 {
		t.Errorf("status written under another controller's Gateway: %+v", pool.Status.Parents)
	}
}

// With no sectionName or port, a pool is served on the first HTTP or HTTPS
// listener its route attaches to - not on the first HTTP one, which may not
// take the route.
func TestPoolIsServedOnTheFirstListenerThatTakesItsRoute(t *testing.T) {
	m := reconcileManager(testPool(nil))
	all := v1.NamespacesFromAll
	createGateway(t, m, otherTeamsGateway("loxilb.io/loxilb",
		v1.Listener{Name: "team-only", Port: 8080, Protocol: v1.HTTPProtocolType},
		v1.Listener{Name: "shared", Port: 8081, Protocol: v1.HTTPProtocolType, AllowedRoutes: fromNamespaces(all, nil)}))
	team := v1.Namespace("team")
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
		poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "team-gw", Namespace: &team})))

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	services := ownedServices(t, m)
	if len(services) != 1 || services[0].Spec.Ports[0].Port != 8081 {
		t.Fatalf("want one Service on the listener that takes the route (8081), got %+v", services)
	}
	if generated, decided := m.Generated(&services[0]); !generated || !decided {
		t.Errorf("Generated = %v (decided %v) for the Service just made, want true", generated, decided)
	}
}

// The load-balancer manager shares a Gateway listener only with a Service this
// manager made, as it made it. The label alone is something any Service can
// carry.
func TestGenerated(t *testing.T) {
	m := reconcileManager(testPool(nil))
	labelled := func(name string, mutate func(*corev1.Service)) *corev1.Service {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm", Labels: map[string]string{poolOwnerLabel: "vllm-qwen3"}},
			Spec: corev1.ServiceSpec{LoadBalancerIP: "123.123.123.1",
				Ports: []corev1.ServicePort{{Name: "inference", Port: 80, Protocol: corev1.ProtocolTCP}}},
		}
		if mutate != nil {
			mutate(svc)
		}
		return svc
	}

	if generated, decided := m.Generated(labelled("vllm-qwen3-inference", nil)); generated || decided {
		t.Errorf("before the pool is reconciled: Generated = %v (decided %v), want undecided", generated, decided)
	}
	ghost := labelled("ghost", nil)
	ghost.Labels[poolOwnerLabel] = "no-such-pool"
	if generated, decided := m.Generated(ghost); generated || !decided {
		t.Errorf("for a pool that does not exist: Generated = %v (decided %v), want false and decided", generated, decided)
	}

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	services := ownedServices(t, m)
	if len(services) != 1 {
		t.Fatalf("want one Service, got %v", serviceNames(services))
	}
	name := services[0].Name

	cases := []struct {
		name string
		svc  *corev1.Service
		want bool
	}{
		{"as made", &services[0], true},
		{"another Gateway's address", labelled(name, func(s *corev1.Service) { s.Spec.LoadBalancerIP = "123.123.123.9" }), false},
		{"another port", labelled(name, func(s *corev1.Service) { s.Spec.Ports[0].Port = 8443 }), false},
		{"a port added", labelled(name, func(s *corev1.Service) {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "more", Port: 9000, Protocol: corev1.ProtocolTCP})
		}), false},
		{"UDP", labelled(name, func(s *corev1.Service) { s.Spec.Ports[0].Protocol = corev1.ProtocolUDP }), false},
		{"another name", labelled("vllm-qwen3-copy", nil), false},
		{"no label", labelled(name, func(s *corev1.Service) { s.Labels = nil }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if generated, decided := m.Generated(tc.svc); generated != tc.want || !decided {
				t.Errorf("Generated = %v (decided %v), want %v", generated, decided, tc.want)
			}
		})
	}

	// The route goes: so does the Service, and with it what it was made with.
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer())
	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if generated, decided := m.Generated(&services[0]); generated || !decided {
		t.Errorf("after the route is gone: Generated = %v (decided %v), want false", generated, decided)
	}
}

// A listener that takes routes from the namespaces a selector picks reads the
// route's namespace labels.
func TestPoolFollowsANamespaceSelector(t *testing.T) {
	m := reconcileManager(testPool(nil))
	selector := v1.NamespacesFromSelector
	createGateway(t, m, otherTeamsGateway("loxilb.io/loxilb", v1.Listener{Name: "http", Port: 8080, Protocol: v1.HTTPProtocolType,
		AllowedRoutes: fromNamespaces(selector, &metav1.LabelSelector{MatchLabels: map[string]string{"inference": "yes"}})}))
	team := v1.Namespace("team")
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
		poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "team-gw", Namespace: &team})))

	for _, tc := range []struct {
		labels map[string]string
		want   int
	}{
		{nil, 0},
		{map[string]string{"inference": "yes"}, 1},
	} {
		m.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "llm", Labels: tc.labels}}))
		if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if services := ownedServices(t, m); len(services) != tc.want {
			t.Errorf("namespace labels %v: %d Service(s), want %d", tc.labels, len(services), tc.want)
		}
	}
}

// A Gateway readdressed: while the reconcile writes the Service over, it is
// this manager's both as it was and as it is about to be; once the reconcile
// is done, only as it is.
func TestGeneratedServicesWhileTheyAreRewritten(t *testing.T) {
	var g generatedServices
	pool := types.NamespacedName{Namespace: "llm", Name: "vllm-qwen3"}
	at := func(addr string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "vllm-qwen3-inference"},
			Spec: corev1.ServiceSpec{LoadBalancerIP: addr, Ports: []corev1.ServicePort{{Port: 80}}}}
	}
	made := func(addr string) bool {
		ok, _ := g.lookup(pool, "vllm-qwen3-inference", generatedServiceOf(at(addr)))
		return ok
	}

	g.settle(pool, []*corev1.Service{at("10.0.0.1")})
	g.allow(pool, []*corev1.Service{at("10.0.0.2")})
	if !made("10.0.0.1") || !made("10.0.0.2") {
		t.Errorf("while it is rewritten: as it was %v, as it is to be %v; want both", made("10.0.0.1"), made("10.0.0.2"))
	}
	g.settle(pool, []*corev1.Service{at("10.0.0.2")})
	if made("10.0.0.1") || !made("10.0.0.2") {
		t.Errorf("once rewritten: as it was %v, as it is %v; want only as it is", made("10.0.0.1"), made("10.0.0.2"))
	}
}
