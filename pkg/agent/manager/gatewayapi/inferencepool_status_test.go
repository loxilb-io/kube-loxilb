package gatewayapi

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	inffake "sigs.k8s.io/gateway-api-inference-extension/client-go/clientset/versioned/fake"
	infv1lister "sigs.k8s.io/gateway-api-inference-extension/client-go/listers/api/v1"
	sigsv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// statusWrites - the UpdateStatus calls the manager made on pools.
func statusWrites(m *InferencePoolManager) int {
	n := 0
	for _, action := range m.infClient.(*inffake.Clientset).Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

// serviceUpdates - the Service updates the manager made.
func serviceUpdates(m *InferencePoolManager) int {
	n := 0
	for _, action := range m.kubeClient.(*fake.Clientset).Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "services" {
			n++
		}
	}
	return n
}

// refreshPoolCache - put the pool as the API server now has it back into the
// lister, which is what the informer would do after a status write.
func refreshPoolCache(t *testing.T, m *InferencePoolManager) {
	t.Helper()

	pool, err := m.infClient.InferenceV1().InferencePools("llm").Get(context.Background(), "vllm-qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	m.poolLister = infv1lister.NewInferencePoolLister(newIndexer(pool))
}

// A reconcile that finds nothing changed must write nothing: no status, no
// Service. Writing either is what kept every pool reconciling every couple of
// seconds.
func TestReconcileIsQuietWhenNothingChanged(t *testing.T) {
	m := reconcileManager(poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose)), pickerService("epp", nil))

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := statusWrites(m); got != 1 {
		t.Fatalf("first reconcile wrote status %d times, want 1", got)
	}

	refreshPoolCache(t, m)
	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := statusWrites(m); got != 1 {
		t.Errorf("an unchanged reconcile wrote status again (%d writes in total)", got)
	}
	if got := serviceUpdates(m); got != 0 {
		t.Errorf("an unchanged reconcile updated the Service %d times", got)
	}
}

// statusPool - a pool that already carries this controller's conditions under
// the Gateway testTarget names, stamped at a fixed time in the past.
func statusPool(stamp metav1.Time, resolvedStatus metav1.ConditionStatus, resolvedReason, resolvedMessage string) *infv1.InferencePool {
	pool := testPool(nil)
	pool.Status.Parents = []infv1.ParentStatus{{
		ParentRef: infv1.ParentReference{
			Group:     groupOf("gateway.networking.k8s.io"),
			Kind:      "Gateway",
			Name:      "inference-gw",
			Namespace: "llm",
		},
		ControllerName: inferencePoolControllerName,
		Conditions: []metav1.Condition{
			{
				Type: string(infv1.InferencePoolConditionAccepted), Status: metav1.ConditionTrue,
				Reason: string(infv1.InferencePoolReasonAccepted), Message: "managed by kube-loxilb",
				LastTransitionTime: stamp,
			},
			{
				Type: string(infv1.InferencePoolConditionResolvedRefs), Status: resolvedStatus,
				Reason: resolvedReason, Message: resolvedMessage,
				LastTransitionTime: stamp,
			},
		},
	}}
	return pool
}

func resolvedReport(message string) statusReport {
	return statusReport{
		accepted: true, acceptedReason: string(infv1.InferencePoolReasonAccepted), acceptedMessage: "managed by kube-loxilb",
		resolvedRefs: true, resolvedRefsReason: string(infv1.InferencePoolReasonResolvedRefs), resolvedRefsMessage: message,
	}
}

func poolConditions(t *testing.T, m *InferencePoolManager) map[string]metav1.Condition {
	t.Helper()

	pool, err := m.infClient.InferenceV1().InferencePools("llm").Get(context.Background(), "vllm-qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	out := map[string]metav1.Condition{}
	for _, c := range pool.Status.Parents[0].Conditions {
		out[c.Type] = c
	}
	return out
}

func sameTime(a, b metav1.Time) bool { return a.Time.Equal(b.Time) }

func TestUpdateStatusTransitionTimes(t *testing.T) {
	past := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	accepted := string(infv1.InferencePoolConditionAccepted)
	resolved := string(infv1.InferencePoolConditionResolvedRefs)

	t.Run("an identical report writes nothing", func(t *testing.T) {
		pool := statusPool(past, metav1.ConditionTrue, string(infv1.InferencePoolReasonResolvedRefs), "resolved to 10.43.12.7:9002")
		m := testManager()
		m.infClient = inffake.NewSimpleClientset(pool)

		if err := m.updateStatus(context.Background(), pool, []poolTarget{testTarget(nil)}, resolvedReport("resolved to 10.43.12.7:9002")); err != nil {
			t.Fatalf("updateStatus: %v", err)
		}
		if got := statusWrites(m); got != 0 {
			t.Errorf("wrote status %d times for an unchanged report", got)
		}
	})

	t.Run("a new message keeps the time while the status holds", func(t *testing.T) {
		pool := statusPool(past, metav1.ConditionTrue, string(infv1.InferencePoolReasonResolvedRefs), "resolved to 10.43.12.7:9002")
		m := testManager()
		m.infClient = inffake.NewSimpleClientset(pool)

		if err := m.updateStatus(context.Background(), pool, []poolTarget{testTarget(nil)}, resolvedReport("resolved to 10.43.99.9:9002")); err != nil {
			t.Fatalf("updateStatus: %v", err)
		}
		got := poolConditions(t, m)
		if got[resolved].Message != "resolved to 10.43.99.9:9002" {
			t.Errorf("message = %q, want the new address", got[resolved].Message)
		}
		if !sameTime(got[resolved].LastTransitionTime, past) {
			t.Errorf("ResolvedRefs lastTransitionTime = %v, want it kept at %v", got[resolved].LastTransitionTime, past)
		}
	})

	t.Run("a changed status stamps the time, an unchanged one keeps it", func(t *testing.T) {
		pool := statusPool(past, metav1.ConditionFalse, string(infv1.InferencePoolReasonInvalidExtensionRef), "not found")
		m := testManager()
		m.infClient = inffake.NewSimpleClientset(pool)

		if err := m.updateStatus(context.Background(), pool, []poolTarget{testTarget(nil)}, resolvedReport("resolved to 10.43.12.7:9002")); err != nil {
			t.Fatalf("updateStatus: %v", err)
		}
		got := poolConditions(t, m)
		if sameTime(got[resolved].LastTransitionTime, past) {
			t.Error("ResolvedRefs went False -> True but kept its old transition time")
		}
		if !sameTime(got[accepted].LastTransitionTime, past) {
			t.Errorf("Accepted stayed True but its transition time moved to %v", got[accepted].LastTransitionTime)
		}
	})
}

func TestPoolChanged(t *testing.T) {
	base := testPool(map[string]string{epSelectAnnotation: "chwbl"})
	base.ResourceVersion = "100"
	base.Generation = 3

	tests := []struct {
		name   string
		mutate func(*infv1.InferencePool)
		want   bool
	}{
		{"status only", func(p *infv1.InferencePool) {
			p.ResourceVersion = "101"
			p.Status.Parents = []infv1.ParentStatus{{ControllerName: "someone-else"}}
		}, false},
		{"resync", func(p *infv1.InferencePool) {}, true},
		{"spec", func(p *infv1.InferencePool) { p.ResourceVersion = "101"; p.Generation = 4 }, true},
		{"loxilb annotation", func(p *infv1.InferencePool) {
			p.ResourceVersion = "101"
			p.Annotations = map[string]string{epSelectAnnotation: "rr"}
		}, true},
		{"label", func(p *infv1.InferencePool) {
			p.ResourceVersion = "101"
			p.Labels = map[string]string{"team": "a"}
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := base.DeepCopy()
			tt.mutate(cur)
			if got := poolChanged(base, cur); got != tt.want {
				t.Errorf("poolChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceDriftOnlySeesWhatThePoolOwns(t *testing.T) {
	desired, err := testManager().buildServices(testPool(map[string]string{epSelectAnnotation: "chwbl"}), []poolTarget{testTarget(nil)}, pickerResolution{})
	if err != nil {
		t.Fatalf("buildServices: %v", err)
	}

	foreign := desired[0].DeepCopy()
	foreign.Annotations["field.cattle.io/publicEndpoints"] = `[{"port":80}]`
	foreign.Labels["team"] = "a"
	if drift := serviceDrift(foreign, desired[0]); drift != "" {
		t.Errorf("another controller's annotation and label read as drift: %s", drift)
	}

	// A loxilb setting the pool no longer carries was copied there by this
	// manager and has to go.
	stale := desired[0].DeepCopy()
	stale.Annotations["loxilb.io/kv-exact-mode"] = "3"
	if drift := serviceDrift(stale, desired[0]); drift == "" {
		t.Error("a loxilb annotation the pool dropped was not seen as drift")
	}
}

// An update replaces what the manager owns and keeps everything else.
func TestApplyServiceKeepsForeignAnnotations(t *testing.T) {
	ctx := context.Background()
	m := testManager()
	pool := testPool(map[string]string{epSelectAnnotation: "chwbl", "loxilb.io/kv-exact-mode": "3"})

	desired, err := m.buildServices(pool, []poolTarget{testTarget(nil)}, pickerResolution{})
	if err != nil {
		t.Fatalf("buildServices: %v", err)
	}
	if err := m.applyService(ctx, pool, desired[0]); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Another controller decorates the Service.
	svc, _ := m.kubeClient.CoreV1().Services("llm").Get(ctx, "vllm-qwen3-inference", metav1.GetOptions{})
	svc.Annotations["field.cattle.io/publicEndpoints"] = `[{"port":80}]`
	if _, err := m.kubeClient.CoreV1().Services("llm").Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("decorate: %v", err)
	}
	before := serviceUpdates(m)

	// Unchanged pool: nothing to write.
	if err := m.applyService(ctx, pool, desired[0]); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := serviceUpdates(m) - before; got != 0 {
		t.Errorf("an unchanged pool updated a decorated Service %d times", got)
	}

	// The pool drops kv-exact and switches selector.
	changed := testPool(map[string]string{epSelectAnnotation: "rr"})
	desired, err = m.buildServices(changed, []poolTarget{testTarget(nil)}, pickerResolution{})
	if err != nil {
		t.Fatalf("buildServices: %v", err)
	}
	if err := m.applyService(ctx, changed, desired[0]); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := m.kubeClient.CoreV1().Services("llm").Get(ctx, "vllm-qwen3-inference", metav1.GetOptions{})
	if got.Annotations["field.cattle.io/publicEndpoints"] == "" {
		t.Error("the other controller's annotation was removed")
	}
	if _, ok := got.Annotations["loxilb.io/kv-exact-mode"]; ok {
		t.Error("a loxilb annotation the pool dropped is still on the Service")
	}
	if got.Annotations[epSelectAnnotation] != "rr" {
		t.Errorf("%s = %q, want rr", epSelectAnnotation, got.Annotations[epSelectAnnotation])
	}
	if got.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %v", got.Spec.Type)
	}
}

// A pool no route references has no parents. That reads back as nil while
// the merge builds an empty slice, which compared as a change and wrote the
// same empty status on every reconcile.
func TestReconcileUnreferencedPoolWritesNothing(t *testing.T) {
	m := reconcileManager(testPool(nil))
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer())

	for i := 0; i < 2; i++ {
		if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := statusWrites(m); got != 0 {
		t.Errorf("an unreferenced pool with no status was written %d times", got)
	}
}
