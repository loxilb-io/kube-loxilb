package gatewayapi

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

func writtenTCPRoute(t *testing.T, m *l4RouteManager, namespace, name string) *v1alpha2.TCPRoute {
	t.Helper()
	r, err := m.sigsClient.GatewayV1alpha2().TCPRoutes(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	return r
}

// parentStatusFor - this controller's parent status for the parentRef naming
// gateway.
func parentStatusFor(t *testing.T, r *v1alpha2.TCPRoute, gateway string) v1.RouteParentStatus {
	t.Helper()
	for _, p := range r.Status.Parents {
		if p.ControllerName == "loxilb.io/loxilb" && string(p.ParentRef.Name) == gateway {
			return p
		}
	}
	t.Fatalf("no status of ours for gateway %s: %+v", gateway, r.Status.Parents)
	return v1.RouteParentStatus{}
}

func statusConditionIs(t *testing.T, p v1.RouteParentStatus, conditionType v1.RouteConditionType, status metav1.ConditionStatus, reason v1.RouteConditionReason) {
	t.Helper()
	c := meta.FindStatusCondition(p.Conditions, string(conditionType))
	if c == nil {
		t.Fatalf("no %s condition in %+v", conditionType, p.Conditions)
	}
	if c.Status != status || c.Reason != string(reason) {
		t.Errorf("%s = %s/%s (%s), want %s/%s", conditionType, c.Status, c.Reason, c.Message, status, reason)
	}
}

// A TCPRoute says under each parentRef whether it is accepted there and
// whether its backendRefs resolve; its status used to stay empty. It says it
// once: a sync that changes nothing writes nothing.
func TestL4RouteReportsStatus(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) { r.Generation = 4 })
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}}.manager(t)
	syncRoute(t, m, route)

	written := writtenTCPRoute(t, m, "llm", "db")
	parent := parentStatusFor(t, written, "l4-gw")
	statusConditionIs(t, parent, v1.RouteConditionAccepted, metav1.ConditionTrue, v1.RouteReasonAccepted)
	statusConditionIs(t, parent, v1.RouteConditionResolvedRefs, metav1.ConditionTrue, v1.RouteReasonResolvedRefs)
	if c := meta.FindStatusCondition(parent.Conditions, "Accepted"); c.ObservedGeneration != 4 {
		t.Errorf("observedGeneration = %d, want 4", c.ObservedGeneration)
	}

	m.sigsClient.(*sigsfake.Clientset).ClearActions()
	if err := m.sync(l4RouteKey{"llm", "db"}, tcpRouteView(written)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for _, a := range m.sigsClient.(*sigsfake.Clientset).Actions() {
		if a.GetVerb() == "update" {
			t.Errorf("an unchanged status was written again: %v", a)
		}
	}
}

func TestL4RouteReportsAccepted(t *testing.T) {
	second := l4Gateway()
	second.Name = "second-gw"
	theirs := v1.RouteParentStatus{
		ParentRef: v1.ParentReference{Name: "their-gw"}, ControllerName: "example.com/other",
		Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"}},
	}
	route := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Spec.ParentRefs = append(r.Spec.ParentRefs, v1.ParentReference{Name: "second-gw"},
			v1.ParentReference{Name: "l4-gw", SectionName: sectionRef("http").SectionName})
		r.Status.Parents = []v1.RouteParentStatus{theirs}
	})
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway(), second}}.manager(t)
	syncRoute(t, m, route)

	written := writtenTCPRoute(t, m, "llm", "db")
	var byRef = map[string]v1.RouteParentStatus{}
	for _, p := range written.Status.Parents {
		key := string(p.ParentRef.Name)
		if p.ParentRef.SectionName != nil {
			key += "/" + string(*p.ParentRef.SectionName)
		}
		byRef[string(p.ControllerName)+" "+key] = p
	}
	if _, ok := byRef["example.com/other their-gw"]; !ok {
		t.Error("another controller's entry went")
	}
	statusConditionIs(t, byRef["loxilb.io/loxilb l4-gw"], v1.RouteConditionAccepted, metav1.ConditionTrue, v1.RouteReasonAccepted)
	// A Service has one address: the second Gateway does not serve it.
	statusConditionIs(t, byRef["loxilb.io/loxilb second-gw"], v1.RouteConditionAccepted, metav1.ConditionFalse, v1.RouteReasonUnsupportedValue)
	statusConditionIs(t, byRef["loxilb.io/loxilb l4-gw/http"], v1.RouteConditionAccepted, metav1.ConditionFalse, v1.RouteReasonNotAllowedByListeners)
}

// An unknown serviceBehaviour leaves the route's Services as they are, and
// the route says why it is not served.
func TestL4RouteUnknownBehaviourHolds(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) { r.Labels[l4BehaviourLabel] = "replace" })
	existing := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db-lb",
		Labels:      map[string]string{"implementation": implementation},
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-tcp-route": "db"}}}
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, services: []*corev1.Service{existing}}.manager(t)
	syncRoute(t, m, route)

	if serviceGone(t, m, "llm", "db-lb") {
		t.Error("the route's Service went over a mistyped label")
	}
	statusConditionIs(t, parentStatusFor(t, writtenTCPRoute(t, m, "llm", "db"), "l4-gw"),
		v1.RouteConditionAccepted, metav1.ConditionFalse, v1.RouteReasonUnsupportedValue)
}

func TestL4RouteReportsResolvedRefs(t *testing.T) {
	cases := []struct {
		name   string
		route  func(*v1alpha2.TCPRoute)
		reason v1.RouteConditionReason
	}{
		{"a kind that is not a Service", func(r *v1alpha2.TCPRoute) {
			kind := v1.Kind("InferencePool")
			r.Spec.Rules[0].BackendRefs[0].Kind = &kind
		}, v1.RouteReasonInvalidKind},
		{"a Service in another namespace without a grant", func(r *v1alpha2.TCPRoute) {
			r.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
		}, v1.RouteReasonRefNotPermitted},
		// It queues the route when it comes, so it is not an error to retry.
		{"a Service to copy that is not there", func(r *v1alpha2.TCPRoute) {
			r.Labels[l4BehaviourLabel] = l4BehaviourDuplicate
		}, v1.RouteReasonBackendNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := tcpRoute(tc.route)
			m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, grants: staticGrants()}.manager(t)
			syncRoute(t, m, route)
			statusConditionIs(t, parentStatusFor(t, writtenTCPRoute(t, m, "llm", "db"), "l4-gw"),
				v1.RouteConditionResolvedRefs, metav1.ConditionFalse, tc.reason)
		})
	}
}

// A listener counts the TCPRoutes and UDPRoutes it serves, which it used to
// leave out.
func TestL4AttachedRoutes(t *testing.T) {
	second := l4Gateway()
	second.Name = "second-gw"
	routes := []*v1alpha2.TCPRoute{
		tcpRoute(),
		tcpRoute(func(r *v1alpha2.TCPRoute) {
			r.Name = "cache"
			r.Spec.ParentRefs[0].SectionName = sectionRef("tcp").SectionName
		}),
		// Served on second-gw: it does not count on l4-gw.
		tcpRoute(func(r *v1alpha2.TCPRoute) {
			r.Name = "elsewhere"
			r.Spec.ParentRefs = []v1.ParentReference{{Name: "second-gw"}, {Name: "l4-gw"}}
		}),
	}
	m := l4Fixture{routes: routes, gateways: []*v1.Gateway{l4Gateway(), second}}.manager(t)

	counts := m.AttachedRoutes(l4Gateway())
	if counts["tcp"] != 2 || counts["tcp-alt"] != 1 || counts["udp"] != 0 {
		t.Errorf("counts = %v, want tcp 2, tcp-alt 1", counts)
	}

	// The Gateway manager adds them to its own count, and is told of a route
	// that changes.
	gm := gatewayTestManager(t, nil, []*v1.Gateway{l4Gateway()})
	gm.CountRoutesOf(m)
	status := gm.gatewayStatus(l4Gateway(), nil)
	if got := listenerStatusOf(t, status, "tcp").AttachedRoutes; got != 2 {
		t.Errorf("tcp attachedRoutes = %d, want 2", got)
	}
	m.enqueueObject(routes[1])
	if item, ok := queued(t, gm.queue).(*v1.Gateway); !ok || item.Name != "l4-gw" {
		t.Errorf("a route's change queued %v, want its Gateway", item)
	}
}

func TestL4EnqueueServiceRoutes(t *testing.T) {
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{tcpRoute()}, gateways: []*v1.Gateway{l4Gateway()}}.manager(t)

	m.enqueueServiceRoutes(tombstone(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db-lb"}}))
	if item := queued(t, m.queue); item != (l4RouteKey{"llm", "db"}) {
		t.Errorf("the Service queued %v, want llm/db", item)
	}
	m.enqueueServiceRoutes(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "db-lb"}})
	if item := queued(t, m.queue); item != nil {
		t.Errorf("a Service of the name in another namespace queued %v", item)
	}
}
