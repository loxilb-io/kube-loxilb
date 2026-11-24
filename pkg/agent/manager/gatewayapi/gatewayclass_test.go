package gatewayapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

// pendingGatewayClass - a GatewayClass as its CRD defaults it: Accepted
// Unknown, Pending, "Waiting for controller".
func pendingGatewayClass(controller string, generation int64, stamp metav1.Time) *v1.GatewayClass {
	return &v1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "loxilb-gc", Generation: generation},
		Spec:       v1.GatewayClassSpec{ControllerName: v1.GatewayController(controller)},
		Status: v1.GatewayClassStatus{Conditions: []metav1.Condition{{
			Type: "Accepted", Status: metav1.ConditionUnknown, Reason: "Pending",
			Message: "Waiting for controller", LastTransitionTime: stamp,
		}}},
	}
}

func gatewayClassManager(gc *v1.GatewayClass) *GatewayClassManager {
	return &GatewayClassManager{
		sigsClient:    sigsfake.NewSimpleClientset(gc),
		networkConfig: &config.NetworkConfig{LoxilbGatewayClass: "loxilb.io/loxilb"},
	}
}

func gatewayClassStatusWrites(m *GatewayClassManager) int {
	n := 0
	for _, action := range m.sigsClient.(*sigsfake.Clientset).Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

func storedGatewayClass(t *testing.T, m *GatewayClassManager) *v1.GatewayClass {
	t.Helper()
	gc, err := m.sigsClient.GatewayV1().GatewayClasses().Get(context.Background(), "loxilb-gc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get GatewayClass: %v", err)
	}
	return gc
}

// Accepted is set whole: True, with the reason Accepted rather than the
// Pending the CRD put there, and the generation it was accepted at.
func TestAGatewayClassIsAcceptedWithItsReasonAndGeneration(t *testing.T) {
	past := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	gc := pendingGatewayClass("loxilb.io/loxilb", 3, past)
	m := gatewayClassManager(gc)

	if err := m.updateGatewayClass(gc); err != nil {
		t.Fatalf("updateGatewayClass: %v", err)
	}
	got := meta.FindStatusCondition(storedGatewayClass(t, m).Status.Conditions, "Accepted")
	if got == nil {
		t.Fatal("no Accepted condition")
	}
	if got.Status != metav1.ConditionTrue || got.Reason != string(v1.GatewayClassReasonAccepted) || got.ObservedGeneration != 3 {
		t.Errorf("Accepted = %s/%s, observedGeneration %d, want True/Accepted at 3", got.Status, got.Reason, got.ObservedGeneration)
	}
	if got.Message == "Waiting for controller" {
		t.Errorf("message is still %q", got.Message)
	}
	if !got.LastTransitionTime.After(past.Time) {
		t.Errorf("lastTransitionTime = %v, want it moved from Unknown to True", got.LastTransitionTime)
	}
	// The lister's object is the informer's cache, and is not changed.
	if c := meta.FindStatusCondition(gc.Status.Conditions, "Accepted"); c.Status != metav1.ConditionUnknown {
		t.Errorf("the GatewayClass handed in was changed: Accepted %s", c.Status)
	}
}

// An accepted GatewayClass is not written again, and its transition time
// stays; a change of its spec moves only the generation it was accepted at.
func TestAnAcceptedGatewayClassKeepsItsTransitionTime(t *testing.T) {
	past := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	gc := pendingGatewayClass("loxilb.io/loxilb", 1, past)
	gc.Status.Conditions[0] = metav1.Condition{
		Type: "Accepted", Status: metav1.ConditionTrue, Reason: string(v1.GatewayClassReasonAccepted),
		Message: "accepted by kube-loxilb", ObservedGeneration: 1, LastTransitionTime: past,
	}
	m := gatewayClassManager(gc)

	if err := m.updateGatewayClass(gc); err != nil {
		t.Fatalf("updateGatewayClass: %v", err)
	}
	if n := gatewayClassStatusWrites(m); n != 0 {
		t.Errorf("an accepted GatewayClass was written %d times", n)
	}

	bumped := gc.DeepCopy()
	bumped.Generation = 2
	if err := m.updateGatewayClass(bumped); err != nil {
		t.Fatalf("updateGatewayClass after a spec change: %v", err)
	}
	got := meta.FindStatusCondition(storedGatewayClass(t, m).Status.Conditions, "Accepted")
	if got.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d after the spec change, want 2", got.ObservedGeneration)
	}
	if !got.LastTransitionTime.Equal(&past) {
		t.Errorf("lastTransitionTime = %v, want it kept at %v: the status did not change", got.LastTransitionTime, past)
	}
}

// A GatewayClass with no condition yet gets Accepted.
func TestAGatewayClassWithoutConditionsIsAccepted(t *testing.T) {
	gc := pendingGatewayClass("loxilb.io/loxilb", 1, metav1.Now())
	gc.Status.Conditions = nil
	m := gatewayClassManager(gc)

	if err := m.updateGatewayClass(gc); err != nil {
		t.Fatalf("updateGatewayClass: %v", err)
	}
	if got := meta.FindStatusCondition(storedGatewayClass(t, m).Status.Conditions, "Accepted"); got == nil || got.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v, want True", got)
	}
}

// Another controller's GatewayClass is left as it is.
func TestAnotherControllersGatewayClassIsLeftAlone(t *testing.T) {
	gc := pendingGatewayClass("example.com/other", 1, metav1.Now())
	m := gatewayClassManager(gc)

	if err := m.updateGatewayClass(gc); err != nil {
		t.Fatalf("updateGatewayClass: %v", err)
	}
	if n := gatewayClassStatusWrites(m); n != 0 {
		t.Errorf("another controller's GatewayClass was written %d times", n)
	}
}

// A failed write is returned, so that the GatewayClass is queued again.
func TestAFailedGatewayClassWriteIsRetried(t *testing.T) {
	gc := pendingGatewayClass("loxilb.io/loxilb", 1, metav1.Now())
	m := gatewayClassManager(gc)
	m.sigsClient.(*sigsfake.Clientset).PrependReactor("update", "gatewayclasses",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("conflict")
		})

	if err := m.updateGatewayClass(gc); err == nil {
		t.Error("updateGatewayClass = nil after a failed write, want the error back for a requeue")
	}
}

// A condition from a later generation than the copy at hand is not changed:
// the copy is stale.
func TestAGatewayClassStatusFromALaterGenerationIsLeftAlone(t *testing.T) {
	gc := pendingGatewayClass("loxilb.io/loxilb", 3, metav1.Now())
	gc.Status.Conditions[0].ObservedGeneration = 5
	m := gatewayClassManager(gc)

	if err := m.updateGatewayClass(gc); err != nil {
		t.Fatalf("updateGatewayClass: %v", err)
	}
	if n := gatewayClassStatusWrites(m); n != 0 {
		t.Errorf("a condition of generation 5 was written from a copy of generation 3 (%d writes)", n)
	}
}
