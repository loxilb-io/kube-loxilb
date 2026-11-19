package gatewayapi

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func listenerStatusOf(t *testing.T, status v1.GatewayStatus, name string) v1.ListenerStatus {
	t.Helper()
	for _, ls := range status.Listeners {
		if string(ls.Name) == name {
			return ls
		}
	}
	t.Fatalf("no status for listener %s", name)
	return v1.ListenerStatus{}
}

func conditionIs(t *testing.T, what string, conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(conditions, conditionType)
	if c == nil {
		t.Errorf("%s: no %s condition", what, conditionType)
		return
	}
	if c.Status != status || c.Reason != reason {
		t.Errorf("%s: %s = %s/%s (%s), want %s/%s", what, conditionType, c.Status, c.Reason, c.Message, status, reason)
	}
}

func addressed(gw *v1.Gateway) *v1.Gateway {
	gw.Spec.Addresses = []v1.GatewaySpecAddress{{Value: "10.10.10.1"}}
	return gw
}

func TestGatewayStatusListeners(t *testing.T) {
	kinds := listener("only-tcp-routes", v1.HTTPProtocolType, 8080, "")
	kinds.AllowedRoutes = &v1.AllowedRoutes{Kinds: []v1.RouteGroupKind{{Kind: kindTCPRoute}}}
	gw := addressed(&v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "gw", Generation: 2},
		Spec: v1.GatewaySpec{GatewayClassName: testClass, Listeners: []v1.Listener{
			listener("http", v1.HTTPProtocolType, 80, ""),
			listener("http-b", v1.HTTPProtocolType, 80, "b.example.com"),
			listener("http-again", v1.HTTPProtocolType, 80, ""),
			withCertificate(listener("https-on-80", v1.HTTPSProtocolType, 80, "c.example.com"), "cert"),
			listener("tcp", v1.TCPProtocolType, 5432, ""),
			listener("udp", v1.UDPProtocolType, 5432, ""),
			listener("tls", v1.TLSProtocolType, 6443, ""),
			withCertificate(listener("https", v1.HTTPSProtocolType, 443, ""), "cert"),
			listener("https-without-cert", v1.HTTPSProtocolType, 8443, ""),
			kinds,
		}},
	})
	gm := gatewayTestManager(t, nil, nil)

	status := gm.gatewayStatus(gw, nil)

	cases := []struct {
		listener   string
		accepted   string
		conflicted string
		resolved   string
		programmed metav1.ConditionStatus
	}{
		{"http", "Accepted", "NoConflicts", "ResolvedRefs", metav1.ConditionTrue},
		{"http-b", "Accepted", "NoConflicts", "ResolvedRefs", metav1.ConditionTrue},
		{"http-again", "Accepted", "HostnameConflict", "ResolvedRefs", metav1.ConditionFalse},
		{"https-on-80", "Accepted", "ProtocolConflict", "ResolvedRefs", metav1.ConditionFalse},
		{"tcp", "Accepted", "NoConflicts", "ResolvedRefs", metav1.ConditionTrue},
		{"udp", "Accepted", "NoConflicts", "ResolvedRefs", metav1.ConditionTrue},
		{"tls", "UnsupportedProtocol", "NoConflicts", "ResolvedRefs", metav1.ConditionFalse},
		{"https", "Accepted", "NoConflicts", "ResolvedRefs", metav1.ConditionTrue},
		{"https-without-cert", "Accepted", "NoConflicts", "InvalidCertificateRef", metav1.ConditionFalse},
		{"only-tcp-routes", "Accepted", "NoConflicts", "InvalidRouteKinds", metav1.ConditionFalse},
	}
	for _, tc := range cases {
		ls := listenerStatusOf(t, status, tc.listener)
		acceptedStatus := metav1.ConditionTrue
		if tc.accepted != "Accepted" {
			acceptedStatus = metav1.ConditionFalse
		}
		conflictedStatus := metav1.ConditionFalse
		if tc.conflicted != "NoConflicts" {
			conflictedStatus = metav1.ConditionTrue
		}
		resolvedStatus := metav1.ConditionTrue
		if tc.resolved != "ResolvedRefs" {
			resolvedStatus = metav1.ConditionFalse
		}
		conditionIs(t, tc.listener, ls.Conditions, "Accepted", acceptedStatus, tc.accepted)
		conditionIs(t, tc.listener, ls.Conditions, "Conflicted", conflictedStatus, tc.conflicted)
		conditionIs(t, tc.listener, ls.Conditions, "ResolvedRefs", resolvedStatus, tc.resolved)
		if c := meta.FindStatusCondition(ls.Conditions, "Programmed"); c == nil || c.Status != tc.programmed {
			t.Errorf("%s: Programmed = %v, want %s", tc.listener, c, tc.programmed)
		}
		for _, c := range ls.Conditions {
			if c.ObservedGeneration != 2 {
				t.Errorf("%s: %s observedGeneration = %d, want 2", tc.listener, c.Type, c.ObservedGeneration)
			}
		}
	}

	if kinds := listenerStatusOf(t, status, "http").SupportedKinds; len(kinds) != 1 || kinds[0].Kind != kindHTTPRoute {
		t.Errorf("http supportedKinds = %+v, want HTTPRoute", kinds)
	}
	if kinds := listenerStatusOf(t, status, "only-tcp-routes").SupportedKinds; kinds == nil || len(kinds) != 0 {
		t.Errorf("only-tcp-routes supportedKinds = %+v, want an empty list", kinds)
	}

	conditionIs(t, "gateway", status.Conditions, "Accepted", metav1.ConditionTrue, string(v1.GatewayReasonListenersNotValid))
	conditionIs(t, "gateway", status.Conditions, "Programmed", metav1.ConditionTrue, string(v1.GatewayReasonProgrammed))
	if len(status.Addresses) != 1 || status.Addresses[0].Value != "10.10.10.1" {
		t.Errorf("addresses = %+v, want the spec's", status.Addresses)
	}
}

func TestGatewayStatusProgrammed(t *testing.T) {
	gm := gatewayTestManager(t, nil, nil)

	status := gm.gatewayStatus(webGateway(), nil)
	conditionIs(t, "no address", status.Conditions, "Programmed", metav1.ConditionFalse, string(v1.GatewayReasonAddressNotAssigned))
	conditionIs(t, "no address, listener", listenerStatusOf(t, status, "http").Conditions, "Programmed",
		metav1.ConditionFalse, string(v1.ListenerReasonPending))

	status = gm.gatewayStatus(addressed(webGateway()), fmt.Errorf("refused"))
	conditionIs(t, "ingress service refused", status.Conditions, "Programmed", metav1.ConditionFalse, string(v1.GatewayReasonInvalid))

	status = gm.gatewayStatus(addressed(webGateway()), nil)
	conditionIs(t, "all well", status.Conditions, "Accepted", metav1.ConditionTrue, string(v1.GatewayReasonAccepted))
	conditionIs(t, "all well", status.Conditions, "Programmed", metav1.ConditionTrue, string(v1.GatewayReasonProgrammed))
}

func TestGatewayStatusAttachedRoutes(t *testing.T) {
	section := v1.SectionName("http")
	routes := []*v1.HTTPRoute{
		// No sectionName: both listeners, counted once each though it names
		// the Gateway twice.
		routeNaming("llm", "both", v1.ParentReference{Name: "web-gw"}, v1.ParentReference{Name: "web-gw", SectionName: &section}),
		routeNaming("llm", "http-only", v1.ParentReference{Name: "web-gw", SectionName: &section}),
		// The listeners take routes from their own namespace only.
		routeNaming("apps", "elsewhere", v1.ParentReference{Name: "web-gw", Namespace: namespacePtr("llm")}),
		routeNaming("llm", "another-gateway", v1.ParentReference{Name: "other-gw"}),
	}
	objs := make([]interface{}, 0, len(routes))
	for _, r := range routes {
		objs = append(objs, r)
	}
	gm := gatewayTestManager(t, nil, nil)
	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(objs...))

	status := gm.gatewayStatus(addressed(webGateway()), nil)
	if got := listenerStatusOf(t, status, "http").AttachedRoutes; got != 2 {
		t.Errorf("http attachedRoutes = %d, want 2", got)
	}
	if got := listenerStatusOf(t, status, "https").AttachedRoutes; got != 1 {
		t.Errorf("https attachedRoutes = %d, want 1", got)
	}
}

// The status is written when it changes and only then. Every reconcile used
// to stamp the conditions with the time of the reconcile, so each one was a
// write, though nothing had changed.
func TestUpdateGatewayStatusWritesOnlyChanges(t *testing.T) {
	gw := addressed(webGateway())
	gm := gatewayTestManager(t, nil, []*v1.Gateway{gw})
	ctx := context.Background()

	if err := gm.updateGatewayStatus(ctx, gw, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	written, err := gm.sigsClient.GatewayV1().Gateways(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	conditionIs(t, "written", written.Status.Conditions, "Programmed", metav1.ConditionTrue, string(v1.GatewayReasonProgrammed))

	gm.sigsClient.(*sigsfake.Clientset).ClearActions()
	if err := gm.updateGatewayStatus(ctx, written, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}
	for _, action := range gm.sigsClient.(*sigsfake.Clientset).Actions() {
		if action.GetVerb() == "update" {
			t.Errorf("an unchanged status was written again: %v", action)
		}
	}

	// A listener that loses its certificate is written as such.
	changed := written.DeepCopy()
	changed.Spec.Listeners[1].TLS = nil
	if err := gm.updateGatewayStatus(ctx, changed, nil); err != nil {
		t.Fatalf("third write: %v", err)
	}
	again, _ := gm.sigsClient.GatewayV1().Gateways(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
	conditionIs(t, "certificate gone", listenerStatusOf(t, again.Status, "https").Conditions, "ResolvedRefs",
		metav1.ConditionFalse, string(v1.ListenerReasonInvalidCertificateRef))
}

// A status write that fails used to hand the ingress service the empty
// Gateway the client returns with the error, and the reconcile failed on
// that, saying the Gateway had no address. A conflict - the cache behind the
// manager's own last write - is the usual way to get there.
func TestCreateGatewaySurvivesAFailedStatusWrite(t *testing.T) {
	gw := addressed(webGateway())

	t.Run("a conflict is tried once more", func(t *testing.T) {
		gm := gatewayTestManager(t, nil, []*v1.Gateway{gw})
		conflicts := 1
		gm.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "gateways",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "status" && conflicts > 0 {
					conflicts--
					return true, nil, errors.NewConflict(schema.GroupResource{Resource: "gateways"}, gw.Name, fmt.Errorf("stale"))
				}
				return false, nil, nil
			})
		if err := gm.createGateway(gw); err != nil {
			t.Fatalf("createGateway: %v", err)
		}
		written, _ := gm.sigsClient.GatewayV1().Gateways(gw.Namespace).Get(context.Background(), gw.Name, metav1.GetOptions{})
		conditionIs(t, "after the conflict", written.Status.Conditions, "Programmed", metav1.ConditionTrue, string(v1.GatewayReasonProgrammed))
	})

	t.Run("another failure is retried", func(t *testing.T) {
		gm := gatewayTestManager(t, nil, []*v1.Gateway{gw})
		gm.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "gateways",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.NewServiceUnavailable("later")
			})
		if err := gm.createGateway(gw); err == nil {
			t.Error("createGateway = nil, want the failure so the Gateway is retried")
		}
		if _, err := gm.kubeClient.CoreV1().Services(gw.Namespace).Get(context.Background(), gw.Name+"-ingress-service", metav1.GetOptions{}); err != nil {
			t.Errorf("the ingress service waited on the status: %v", err)
		}
	})
}

// The status is worked out on a copy. Setting a listener's conditions in
// place in the Gateway's own status would also make it look written already.
func TestGatewayStatusLeavesTheGatewayAlone(t *testing.T) {
	gm := gatewayTestManager(t, nil, nil)
	gw := addressed(webGateway())
	gw.Status = gm.gatewayStatus(gw, nil)
	before := gw.Status.DeepCopy()

	gw.Spec.Listeners[1].TLS = nil
	_ = gm.gatewayStatus(gw, nil)
	if !reflect.DeepEqual(*before, gw.Status) {
		t.Errorf("gatewayStatus wrote into the Gateway's status")
	}
}
