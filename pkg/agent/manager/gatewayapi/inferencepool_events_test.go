package gatewayapi

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func newTestQueue() workqueue.TypedRateLimitingInterface[InferencePoolQueueEntry] {
	return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[InferencePoolQueueEntry]())
}

// drainQueue - everything queued, as a set.
func drainQueue(q workqueue.TypedRateLimitingInterface[InferencePoolQueueEntry]) map[InferencePoolQueueEntry]bool {
	out := map[InferencePoolQueueEntry]bool{}
	for q.Len() > 0 {
		entry, _ := q.Get()
		out[entry] = true
		q.Done(entry)
	}
	return out
}

func poolRoute(namespace, name, pool string, parent v1.ParentReference) *v1.HTTPRoute {
	group := v1.Group(inferencePoolGroup)
	kind := v1.Kind(inferencePoolKind)
	return &v1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.HTTPRouteSpec{
			CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: []v1.ParentReference{parent}},
			Rules: []v1.HTTPRouteRule{{BackendRefs: []v1.HTTPBackendRef{{BackendRef: v1.BackendRef{
				BackendObjectReference: v1.BackendObjectReference{Group: &group, Kind: &kind, Name: v1.ObjectName(pool)},
			}}}}},
		},
	}
}

func TestEnqueueGatewayPools(t *testing.T) {
	llm := v1.Namespace("llm")
	m := testManager()
	m.queue = newTestQueue()
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
		poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "inference-gw"}),
		poolRoute("llm", "other-route", "other-pool", v1.ParentReference{Name: "other-gw"}),
		// From another namespace, naming the Gateway's namespace explicitly.
		poolRoute("apps", "apps-route", "apps-pool", v1.ParentReference{Name: "inference-gw", Namespace: &llm}),
	))

	m.enqueueGatewayPools(testTarget(nil).gateway)

	got := drainQueue(m.queue)
	want := map[InferencePoolQueueEntry]bool{
		{Namespace: "llm", Name: "vllm-qwen3"}: true,
		{Namespace: "apps", Name: "apps-pool"}: true,
	}
	if len(got) != len(want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
	for entry := range want {
		if !got[entry] {
			t.Errorf("%v not queued; got %v", entry, got)
		}
	}

	// A tombstone for a deleted Gateway reaches the same pools.
	m.enqueueGatewayPools(cache.DeletedFinalStateUnknown{Key: "llm/inference-gw", Obj: testTarget(nil).gateway})
	if got := drainQueue(m.queue); len(got) != 2 {
		t.Errorf("tombstone queued %v, want the same two pools", got)
	}
}

func TestGatewayChanged(t *testing.T) {
	base := testTarget(nil).gateway.DeepCopy()
	base.ResourceVersion = "10"
	base.Generation = 2

	tests := []struct {
		name   string
		mutate func(*v1.Gateway)
		want   bool
	}{
		{"status conditions only", func(g *v1.Gateway) {
			g.ResourceVersion = "11"
			g.Status.Conditions = []metav1.Condition{{Type: "Programmed", Status: metav1.ConditionTrue}}
		}, false},
		{"readdressed", func(g *v1.Gateway) {
			g.ResourceVersion = "11"
			g.Status.Addresses = []v1.GatewayStatusAddress{{Value: "123.123.123.2"}}
		}, true},
		{"listeners", func(g *v1.Gateway) { g.ResourceVersion = "11"; g.Generation = 3 }, true},
		{"resync", func(g *v1.Gateway) {}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := base.DeepCopy()
			tt.mutate(cur)
			if got := gatewayChanged(base, cur); got != tt.want {
				t.Errorf("gatewayChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

func ownedService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vllm-qwen3-inference", Namespace: "llm", ResourceVersion: "5",
			Labels:      map[string]string{poolOwnerLabel: "vllm-qwen3", "implementation": implementation},
			Annotations: map[string]string{epSelectAnnotation: "chwbl"},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerIP: "123.123.123.1"},
	}
}

func TestOwnedServiceEdited(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Service)
		want   bool
	}{
		{"external IP written to status", func(s *corev1.Service) {
			s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "123.123.123.1"}}
		}, false},
		{"another controller's annotation", func(s *corev1.Service) {
			s.Annotations["field.cattle.io/publicEndpoints"] = "[]"
		}, false},
		{"spec edited by hand", func(s *corev1.Service) { s.Spec.LoadBalancerIP = "10.0.0.9" }, true},
		{"loxilb annotation edited by hand", func(s *corev1.Service) { s.Annotations[epSelectAnnotation] = "rr" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := ownedService()
			tt.mutate(cur)
			if got := ownedServiceEdited(ownedService(), cur); got != tt.want {
				t.Errorf("ownedServiceEdited = %v, want %v", got, tt.want)
			}
		})
	}

	plain := ownedService()
	delete(plain.Labels, poolOwnerLabel)
	edited := plain.DeepCopy()
	edited.Spec.LoadBalancerIP = "10.0.0.9"
	if ownedServiceEdited(plain, edited) {
		t.Error("a Service no pool made counted as this manager's")
	}
}

func TestEnqueueOwnerPool(t *testing.T) {
	m := testManager()
	m.queue = newTestQueue()

	m.enqueueOwnerPool(cache.DeletedFinalStateUnknown{Key: "llm/vllm-qwen3-inference", Obj: ownedService()})
	if got := drainQueue(m.queue); !got[InferencePoolQueueEntry{Namespace: "llm", Name: "vllm-qwen3"}] || len(got) != 1 {
		t.Errorf("queued %v, want the owner pool only", got)
	}

	m.enqueueOwnerPool(pickerService("epp", nil))
	if got := drainQueue(m.queue); len(got) != 0 {
		t.Errorf("a Service no pool made queued %v", got)
	}
}

// Before, a status write every couple of seconds covered for these by
// accident. Now the events have to: a Service deleted by hand comes back, and
// a readdressed Gateway moves the Service with it.
func TestReconcileRecovers(t *testing.T) {
	ctx := context.Background()

	t.Run("a generated Service deleted by hand", func(t *testing.T) {
		m := reconcileManager(testPool(nil))
		m.queue = newTestQueue()
		if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		svc, err := m.kubeClient.CoreV1().Services("llm").Get(ctx, "vllm-qwen3-inference", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get service: %v", err)
		}
		if err := m.kubeClient.CoreV1().Services("llm").Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete: %v", err)
		}

		m.enqueueOwnerPool(svc) // what the Service informer's delete handler does
		for entry := range drainQueue(m.queue) {
			if err := m.reconcile(entry.Namespace, entry.Name); err != nil {
				t.Fatalf("reconcile %v: %v", entry, err)
			}
		}
		if len(ownedServices(t, m)) != 1 {
			t.Error("the deleted Service was not recreated")
		}
	})

	t.Run("a readdressed Gateway", func(t *testing.T) {
		m := reconcileManager(testPool(nil))
		m.queue = newTestQueue()
		m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(
			poolRoute("llm", "llm-route", "vllm-qwen3", v1.ParentReference{Name: "inference-gw"})))
		if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		gw, err := m.sigsClient.GatewayV1().Gateways("llm").Get(ctx, "inference-gw", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get gateway: %v", err)
		}
		gw.Status.Addresses = []v1.GatewayStatusAddress{{Value: "123.123.123.2"}}
		if _, err := m.sigsClient.GatewayV1().Gateways("llm").UpdateStatus(ctx, gw, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("readdress: %v", err)
		}

		m.enqueueGatewayPools(gw) // what the Gateway informer's update handler does
		for entry := range drainQueue(m.queue) {
			if err := m.reconcile(entry.Namespace, entry.Name); err != nil {
				t.Fatalf("reconcile %v: %v", entry, err)
			}
		}
		services := ownedServices(t, m)
		if len(services) != 1 || services[0].Spec.LoadBalancerIP != "123.123.123.2" {
			t.Errorf("Service did not follow the Gateway's new address: %+v", services)
		}
	})
}
