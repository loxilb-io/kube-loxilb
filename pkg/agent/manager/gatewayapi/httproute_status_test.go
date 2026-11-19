package gatewayapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1lister "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

// statusManager - servingManager with the route in the API server, so its
// status can be written, and the Service web in llm.
func statusManager(route *v1.HTTPRoute, gateways ...*v1.Gateway) *HTTPRouteManager {
	h := servingManager(gateways)
	h.sigsClient = sigsfake.NewSimpleClientset(route)
	h.serviceLister = corev1lister.NewServiceLister(newIndexer(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "web"}}))
	h.servesInferencePools = true
	return h
}

func writtenRoute(t *testing.T, h *HTTPRouteManager, route *v1.HTTPRoute) *v1.HTTPRoute {
	t.Helper()
	got, err := h.sigsClient.GatewayV1().HTTPRoutes(route.Namespace).Get(context.Background(), route.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	return got
}

func routeStatusWrites(h *HTTPRouteManager) int {
	n := 0
	for _, action := range h.sigsClient.(*sigsfake.Clientset).Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

// ours - the one parent status this controller wrote.
func ours(t *testing.T, route *v1.HTTPRoute) v1.RouteParentStatus {
	t.Helper()
	var found []v1.RouteParentStatus
	for _, parent := range route.Status.Parents {
		if parent.ControllerName == "loxilb.io/loxilb" {
			found = append(found, parent)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d parent statuses of ours, want 1: %+v", len(found), route.Status.Parents)
	}
	return found[0]
}

func checkCondition(t *testing.T, parent v1.RouteParentStatus, conditionType v1.RouteConditionType,
	status metav1.ConditionStatus, reason v1.RouteConditionReason, messagePart string) {
	t.Helper()
	c := meta.FindStatusCondition(parent.Conditions, string(conditionType))
	if c == nil {
		t.Fatalf("no %s condition in %+v", conditionType, parent.Conditions)
	}
	if c.Status != status || c.Reason != string(reason) || !strings.Contains(c.Message, messagePart) {
		t.Errorf("%s = %s/%s %q, want %s/%s containing %q", conditionType, c.Status, c.Reason, c.Message, status, reason, messagePart)
	}
}

// A route on a Gateway of this controller's says so under its parentRef,
// and says it once: a sync that finds nothing changed writes nothing.
func TestSyncHTTPRouteReportsStatus(t *testing.T) {
	route := webRoute()
	route.Generation = 3
	h := statusManager(route, webGateway())

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	written := writtenRoute(t, h, route)
	parent := ours(t, written)
	if parent.ParentRef.Name != "web-gw" {
		t.Errorf("parentRef = %+v, want the route's", parent.ParentRef)
	}
	checkCondition(t, parent, v1.RouteConditionAccepted, metav1.ConditionTrue, v1.RouteReasonAccepted, "")
	checkCondition(t, parent, v1.RouteConditionResolvedRefs, metav1.ConditionTrue, v1.RouteReasonResolvedRefs, "")
	for _, c := range parent.Conditions {
		if c.ObservedGeneration != 3 {
			t.Errorf("%s observedGeneration = %d, want 3", c.Type, c.ObservedGeneration)
		}
	}

	h.sigsClient.(*sigsfake.Clientset).ClearActions()
	if err := h.syncHTTPRoute(written); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n := routeStatusWrites(h); n != 0 {
		t.Errorf("a sync with nothing changed wrote the status %d times", n)
	}
}

// What another controller wrote stays, and what this one wrote for a parent it
// no longer serves goes.
func TestSyncHTTPRouteKeepsOtherControllersStatus(t *testing.T) {
	route := webRoute()
	theirs := v1.RouteParentStatus{
		ParentRef:      v1.ParentReference{Name: "their-gw"},
		ControllerName: "example.com/other",
		Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"}},
	}
	stale := v1.RouteParentStatus{
		ParentRef:      v1.ParentReference{Name: "old-gw"},
		ControllerName: "loxilb.io/loxilb",
		Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted"}},
	}
	route.Status.Parents = []v1.RouteParentStatus{theirs, stale}
	h := statusManager(route, webGateway())

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	written := writtenRoute(t, h, route)
	if len(written.Status.Parents) != 2 || written.Status.Parents[0].ControllerName != "example.com/other" {
		t.Fatalf("parents = %+v, want theirs and one of ours", written.Status.Parents)
	}
	if ours(t, written).ParentRef.Name != "web-gw" {
		t.Error("the entry for the parent no longer served is still there")
	}

	// The Gateway moves to another controller's class: ours goes, theirs stays.
	gw := webGateway()
	gw.Spec.GatewayClassName = "other-gc"
	h2 := statusManager(written, gw)
	if err := h2.syncHTTPRoute(written); err != nil {
		t.Fatalf("sync after the class changed: %v", err)
	}
	again := writtenRoute(t, h2, written)
	if len(again.Status.Parents) != 1 || again.Status.Parents[0].ControllerName != "example.com/other" {
		t.Errorf("parents = %+v, want theirs alone", again.Status.Parents)
	}
}

func TestSyncHTTPRouteReportsAccepted(t *testing.T) {
	cases := []struct {
		name    string
		gateway func(*v1.Gateway)
		route   func(*v1.HTTPRoute)
		status  metav1.ConditionStatus
		reason  v1.RouteConditionReason
		message string
	}{
		{
			name:   "attached",
			status: metav1.ConditionTrue, reason: v1.RouteReasonAccepted,
		},
		{
			name: "from a namespace no listener takes",
			route: func(r *v1.HTTPRoute) {
				r.Namespace = "apps"
				r.Spec.ParentRefs[0].Namespace = namespacePtr("llm")
			},
			status: metav1.ConditionFalse, reason: v1.RouteReasonNotAllowedByListeners, message: "takes routes from namespace llm only",
		},
		{
			name:   "to a listener that does not exist",
			route:  func(r *v1.HTTPRoute) { r.Spec.ParentRefs[0].SectionName = sectionRef("grpc").SectionName },
			status: metav1.ConditionFalse, reason: v1.RouteReasonNoMatchingParent, message: "has no listener grpc",
		},
		{
			name: "only to an HTTPS listener that cannot serve it",
			gateway: func(gw *v1.Gateway) {
				gw.Spec.Listeners[1].Hostname = nil
			},
			route: func(r *v1.HTTPRoute) {
				r.Spec.Hostnames = nil
				r.Spec.ParentRefs[0].SectionName = sectionRef("https").SectionName
			},
			status: metav1.ConditionFalse, reason: v1.RouteReasonUnsupportedValue, message: "terminates TLS only for a named host",
		},
		{
			name: "to that HTTPS listener and one that serves it",
			gateway: func(gw *v1.Gateway) {
				gw.Spec.Listeners[1].Hostname = nil
			},
			route:  func(r *v1.HTTPRoute) { r.Spec.Hostnames = nil },
			status: metav1.ConditionTrue, reason: v1.RouteReasonAccepted, message: "listener https does not serve the route",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := webGateway()
			if tc.gateway != nil {
				tc.gateway(gw)
			}
			route := webRoute()
			if tc.route != nil {
				tc.route(route)
			}
			h := statusManager(route, gw)
			h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("llm", nil), namespace("apps", nil)))

			if err := h.syncHTTPRoute(route); err != nil {
				t.Fatalf("syncHTTPRoute: %v", err)
			}
			checkCondition(t, ours(t, writtenRoute(t, h, route)), v1.RouteConditionAccepted, tc.status, tc.reason, tc.message)
		})
	}
}

func TestSyncHTTPRouteReportsResolvedRefs(t *testing.T) {
	cases := []struct {
		name    string
		backend v1.HTTPBackendRef
		grants  *ReferenceGrants
		pools   bool
		status  metav1.ConditionStatus
		reason  v1.RouteConditionReason
		message string
	}{
		{
			name: "a Service that exists", backend: backendRef("", "", "web"),
			status: metav1.ConditionTrue, reason: v1.RouteReasonResolvedRefs,
		},
		{
			name: "a Service that does not", backend: backendRef("", "", "gone"),
			status: metav1.ConditionFalse, reason: v1.RouteReasonBackendNotFound, message: "Service llm/gone does not exist",
		},
		{
			name: "a Service in another namespace without a grant",
			backend: func() v1.HTTPBackendRef {
				ref := backendRef("", "", "web")
				ref.Namespace = namespacePtr("backends")
				return ref
			}(),
			status: metav1.ConditionFalse, reason: v1.RouteReasonRefNotPermitted, message: "namespace backends",
		},
		{
			name: "a kind this controller does not serve", backend: backendRef("example.com", "Bucket", "b"),
			status: metav1.ConditionFalse, reason: v1.RouteReasonInvalidKind, message: "example.com/Bucket b",
		},
		{
			name: "an InferencePool", backend: backendRef(inferencePoolGroup, inferencePoolKind, "vllm"), pools: true,
			status: metav1.ConditionTrue, reason: v1.RouteReasonResolvedRefs,
		},
		{
			name: "an InferencePool without the extension", backend: backendRef(inferencePoolGroup, inferencePoolKind, "vllm"),
			status: metav1.ConditionFalse, reason: v1.RouteReasonInvalidKind, message: "inference extension is not enabled",
		},
		{
			name: "an InferencePool in another namespace", pools: true,
			backend: func() v1.HTTPBackendRef {
				ref := backendRef(inferencePoolGroup, inferencePoolKind, "vllm")
				ref.Namespace = namespacePtr("models")
				return ref
			}(),
			status: metav1.ConditionFalse, reason: v1.RouteReasonRefNotPermitted, message: "InferencePool models/vllm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := webRoute()
			route.Spec.Rules = append(route.Spec.Rules, v1.HTTPRouteRule{BackendRefs: []v1.HTTPBackendRef{tc.backend}})
			h := statusManager(route, webGateway())
			h.servesInferencePools = tc.pools
			h.referenceGrants = tc.grants

			if err := h.syncHTTPRoute(route); err != nil {
				t.Fatalf("syncHTTPRoute: %v", err)
			}
			checkCondition(t, ours(t, writtenRoute(t, h, route)), v1.RouteConditionResolvedRefs, tc.status, tc.reason, tc.message)
		})
	}
}

// A condition whose status is what it was keeps its transition time.
func TestSyncHTTPRouteKeepsTransitionTimes(t *testing.T) {
	route := webRoute()
	h := statusManager(route, webGateway())
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := writtenRoute(t, h, route)
	accepted := meta.FindStatusCondition(ours(t, first).Conditions, string(v1.RouteConditionAccepted))
	accepted.LastTransitionTime = metav1.NewTime(accepted.LastTransitionTime.Add(-3600e9))
	before := accepted.LastTransitionTime

	changed := first.DeepCopy()
	changed.Spec.Rules[0].BackendRefs[0].Name = "gone"
	if err := h.syncHTTPRoute(changed); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	parent := ours(t, writtenRoute(t, h, route))
	checkCondition(t, parent, v1.RouteConditionResolvedRefs, metav1.ConditionFalse, v1.RouteReasonBackendNotFound, "")
	if got := meta.FindStatusCondition(parent.Conditions, string(v1.RouteConditionAccepted)).LastTransitionTime; !got.Equal(&before) {
		t.Errorf("Accepted moved from %v to %v without changing", before, got)
	}
}

func TestUpdateRouteStatusErrors(t *testing.T) {
	route := webRoute()
	statusOf := func(h *HTTPRouteManager) []v1.RouteParentStatus {
		_, parents, err := h.routeIngresses(route)
		if err != nil {
			t.Fatalf("routeIngresses: %v", err)
		}
		return h.routeParentStatuses(route, parents)
	}

	t.Run("a conflict is tried once more", func(t *testing.T) {
		h := statusManager(route, webGateway())
		conflicts := 1
		h.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "httproutes",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "status" && conflicts > 0 {
					conflicts--
					return true, nil, errors.NewConflict(schema.GroupResource{Resource: "httproutes"}, route.Name, fmt.Errorf("stale"))
				}
				return false, nil, nil
			})
		if err := h.updateRouteStatus(context.Background(), route, statusOf(h)); err != nil {
			t.Fatalf("updateRouteStatus: %v", err)
		}
		ours(t, writtenRoute(t, h, route))
	})

	t.Run("a ClusterRole without httproutes/status is not retried", func(t *testing.T) {
		h := statusManager(route, webGateway())
		h.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "httproutes",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.NewForbidden(schema.GroupResource{Resource: "httproutes"}, route.Name, fmt.Errorf("no"))
			})
		if err := h.updateRouteStatus(context.Background(), route, statusOf(h)); err != nil {
			t.Errorf("updateRouteStatus = %v, want nil so the route is not retried for it", err)
		}
	})

	t.Run("another failure is", func(t *testing.T) {
		h := statusManager(route, webGateway())
		h.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "httproutes",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.NewServiceUnavailable("later")
			})
		if err := h.updateRouteStatus(context.Background(), route, statusOf(h)); err == nil {
			t.Error("updateRouteStatus = nil, want the failure so the route is retried")
		}
	})
}

func TestEnqueueServiceRoutes(t *testing.T) {
	inBackends := webRoute()
	inBackends.Name = "cross"
	inBackends.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
	pool := webRoute()
	pool.Name = "pool"
	pool.Spec.Rules[0].BackendRefs = []v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "web")}
	h := withRouteObjects(httpRouteTestManager(), []*v1.HTTPRoute{webRoute(), inBackends, pool})

	h.enqueueServiceRoutes(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "web"}})
	want := HTTPRouteQueueEntry{"llm", "web"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("Service llm/web queued %v, want only %v", got, want)
	}

	h.enqueueServiceRoutes(tombstone(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "backends", Name: "web"}}))
	want = HTTPRouteQueueEntry{"llm", "cross"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("Service backends/web queued %v, want only %v", got, want)
	}
}
