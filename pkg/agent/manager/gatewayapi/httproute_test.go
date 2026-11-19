package gatewayapi

import (
	"context"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func httpRouteTestManager(objs ...*netv1.Ingress) *HTTPRouteManager {
	client := fake.NewClientset()
	for _, ing := range objs {
		_, _ = client.NetworkingV1().Ingresses(ing.Namespace).Create(context.Background(), ing, metav1.CreateOptions{})
	}
	client.ClearActions()
	return &HTTPRouteManager{kubeClient: client, gatewayProvider: "loxilb.io/loxilb"}
}

func backendRef(group, kind, name string) v1.HTTPBackendRef {
	ref := v1.HTTPBackendRef{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{Name: v1.ObjectName(name)}}}
	if group != "" {
		g := v1.Group(group)
		ref.Group = &g
	}
	if kind != "" {
		k := v1.Kind(kind)
		ref.Kind = &k
	}
	port := v1.PortNumber(80)
	ref.Port = &port
	return ref
}

func routeWith(rules ...[]v1.HTTPBackendRef) *v1.HTTPRoute {
	prefix := v1.PathMatchPathPrefix
	path := "/"
	route := &v1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "llm-route", Namespace: "llm"}}
	for _, refs := range rules {
		route.Spec.Rules = append(route.Spec.Rules, v1.HTTPRouteRule{
			Matches:     []v1.HTTPRouteMatch{{Path: &v1.HTTPPathMatch{Type: &prefix, Value: &path}}},
			BackendRefs: refs,
		})
	}
	return route
}

func ingressCreates(h *HTTPRouteManager) int {
	n := 0
	for _, action := range h.kubeClient.(*fake.Clientset).Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "ingresses" {
			n++
		}
	}
	return n
}

func hostListener() *v1.Listener {
	host := v1.Hostname("llm.example.com")
	return &v1.Listener{Name: "http", Port: 80, Protocol: v1.HTTPProtocolType, Hostname: &host}
}

// A route that only names InferencePools has nothing for an Ingress to carry;
// creating one anyway was refused by the API server on every reconcile.
func TestReconcileIngressSkipsInferencePoolOnlyRoutes(t *testing.T) {
	h := httpRouteTestManager()
	route := routeWith([]v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "vllm-qwen3")})

	if err := h.reconcileIngress(context.Background(), route, testTarget(nil).gateway, hostListener()); err != nil {
		t.Fatalf("reconcileIngress: %v", err)
	}
	if got := ingressCreates(h); got != 0 {
		t.Errorf("tried to create %d Ingresses for a route with only InferencePool backends", got)
	}
}

// A route that used to name a Service and now names only an InferencePool
// leaves no Ingress behind.
func TestReconcileIngressRemovesTheOldIngress(t *testing.T) {
	old := &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "llm-route", Namespace: "llm",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-http-route": "llm-route"},
	}}
	other := &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated", Namespace: "llm",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-http-route": "another-route"},
	}}
	h := httpRouteTestManager(old, other)
	route := routeWith([]v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "vllm-qwen3")})

	if err := h.reconcileIngress(context.Background(), route, testTarget(nil).gateway, hostListener()); err != nil {
		t.Fatalf("reconcileIngress: %v", err)
	}

	list, _ := h.kubeClient.NetworkingV1().Ingresses("llm").List(context.Background(), metav1.ListOptions{})
	names := map[string]bool{}
	for _, ing := range list.Items {
		names[ing.Name] = true
	}
	if names["llm-route"] {
		t.Error("the route's old Ingress is still there")
	}
	if !names["unrelated"] {
		t.Error("another route's Ingress was removed")
	}
}

// In a route that mixes the two, the InferencePool rule is left out instead
// of becoming an IngressRule without paths, which would invalidate the
// whole Ingress.
func TestCreateIngressLeavesOutInferencePoolRules(t *testing.T) {
	h := httpRouteTestManager()
	route := routeWith(
		[]v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "vllm-qwen3")},
		[]v1.HTTPBackendRef{backendRef("", "", "web")},
	)

	ing, err := h.createIngress(context.Background(), route, testTarget(nil).gateway, hostListener())
	if err != nil {
		t.Fatalf("createIngress: %v", err)
	}
	if len(ing.Spec.Rules) != 1 {
		t.Fatalf("got %d IngressRules, want 1 for the Service rule only", len(ing.Spec.Rules))
	}
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 || paths[0].Backend.Service.Name != "web" {
		t.Errorf("paths = %+v, want the one Service backend", paths)
	}
}

// withRouteObjects - point the manager's listers at routes and gateways, and
// give it a queue to watch.
func withRouteObjects(h *HTTPRouteManager, routes []*v1.HTTPRoute, gateways ...*v1.Gateway) *HTTPRouteManager {
	routeObjs := make([]interface{}, 0, len(routes))
	for _, r := range routes {
		routeObjs = append(routeObjs, r)
	}
	gwObjs := make([]interface{}, 0, len(gateways))
	for _, gw := range gateways {
		gwObjs = append(gwObjs, gw)
	}
	h.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(routeObjs...))
	h.gatewayLister = sigs_v1_lister.NewGatewayLister(newIndexer(gwObjs...))
	h.queue = testQueue()
	return h
}

// drained - every route q holds, emptying it.
func drained(q workqueue.RateLimitingInterface) map[HTTPRouteQueueEntry]bool {
	got := map[HTTPRouteQueueEntry]bool{}
	for q.Len() > 0 {
		item, _ := q.Get()
		q.Done(item)
		got[item.(HTTPRouteQueueEntry)] = true
	}
	return got
}

func routeNaming(namespace, name string, refs ...v1.ParentReference) *v1.HTTPRoute {
	return &v1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       v1.HTTPRouteSpec{CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: refs}},
	}
}

// A route applied before its Gateway has nothing to attach to yet. It used to
// fail and back off for up to two minutes; the Gateway queues it instead when
// it comes.
func TestHTTPRouteWaitsForItsGateway(t *testing.T) {
	route := routeWith([]v1.HTTPBackendRef{backendRef("", "", "web")})
	route.Spec.ParentRefs = []v1.ParentReference{{Name: "inference-gw"}}
	h := withRouteObjects(httpRouteTestManager(), []*v1.HTTPRoute{route})

	if err := h.createHTTPRoute(route); err != nil {
		t.Fatalf("createHTTPRoute without its Gateway: %v, want nil", err)
	}
	if got := ingressCreates(h); got != 0 {
		t.Errorf("created %d Ingresses for a route without its Gateway", got)
	}

	h.enqueueGatewayRoutes(&v1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "inference-gw"}})
	want := HTTPRouteQueueEntry{"llm", "llm-route"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("the Gateway queued %v, want only %v", got, want)
	}
}

func TestEnqueueGatewayRoutes(t *testing.T) {
	gwNs := v1.Namespace("llm")
	service := v1.Kind("Service")
	core := v1.Group("")
	routes := []*v1.HTTPRoute{
		routeNaming("llm", "same-namespace", v1.ParentReference{Name: "gw"}),
		routeNaming("apps", "other-namespace", v1.ParentReference{Name: "gw", Namespace: &gwNs}),
		routeNaming("apps", "gateway-of-its-own-namespace", v1.ParentReference{Name: "gw"}),
		routeNaming("llm", "another-gateway", v1.ParentReference{Name: "gw2"}),
		routeNaming("llm", "a-service", v1.ParentReference{Name: "gw", Kind: &service, Group: &core}),
		routeNaming("llm", "two-listeners",
			sectionRef("a"),
			sectionRef("b")),
	}
	gw := &v1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "gw", Generation: 1}}
	want := map[HTTPRouteQueueEntry]bool{
		{"llm", "same-namespace"}:   true,
		{"apps", "other-namespace"}: true,
		{"llm", "two-listeners"}:    true,
	}

	check := func(t *testing.T, got map[HTTPRouteQueueEntry]bool) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("queued %v, want %v", got, want)
		}
		for entry := range want {
			if !got[entry] {
				t.Errorf("did not queue %v", entry)
			}
		}
	}

	t.Run("gateway", func(t *testing.T) {
		h := withRouteObjects(httpRouteTestManager(), routes)
		h.enqueueGatewayRoutes(gw)
		check(t, drained(h.queue))
	})
	t.Run("tombstone", func(t *testing.T) {
		h := withRouteObjects(httpRouteTestManager(), routes)
		h.enqueueGatewayRoutes(tombstone(gw))
		check(t, drained(h.queue))
	})
	t.Run("spec change", func(t *testing.T) {
		h := withRouteObjects(httpRouteTestManager(), routes)
		changed := gw.DeepCopy()
		changed.Generation = 2
		h.gatewayUpdated(gw, changed)
		check(t, drained(h.queue))
	})
	t.Run("status write", func(t *testing.T) {
		h := withRouteObjects(httpRouteTestManager(), routes)
		written := gw.DeepCopy()
		written.Status.Addresses = []v1.GatewayStatusAddress{{Value: "10.0.0.1"}}
		h.gatewayUpdated(gw, written)
		if got := drained(h.queue); len(got) != 0 {
			t.Errorf("a status write queued %v, want nothing", got)
		}
	})
}
