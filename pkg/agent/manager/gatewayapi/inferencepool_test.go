package gatewayapi

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

func testManager() *InferencePoolManager {
	return &InferencePoolManager{
		kubeClient: fake.NewClientset(),
		networkConfig: &config.NetworkConfig{
			LoxilbLoadBalancerClass: "loxilb.io/loxilb",
			LoxilbGatewayClass:      "loxilb.io/loxilb",
		},
		gatewayProvider: "loxilb.io/loxilb",
	}
}

func testPool(annotations map[string]string) *infv1.InferencePool {
	return &infv1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm-qwen3", Namespace: "llm", Annotations: annotations},
		Spec: infv1.InferencePoolSpec{
			Selector:    infv1.LabelSelector{MatchLabels: map[infv1.LabelKey]infv1.LabelValue{"app": "vllm-qwen3"}},
			TargetPorts: []infv1.Port{{Number: 8000}},
		},
	}
}

func testTarget(routeAnnotations map[string]string) poolTarget {
	listenerName := v1.SectionName("http")
	return poolTarget{
		gateway: &v1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "inference-gw", Namespace: "llm"},
			Status: v1.GatewayStatus{
				Addresses: []v1.GatewayStatusAddress{{Value: "123.123.123.1"}},
			},
		},
		listener:         &v1.Listener{Name: listenerName, Port: 80, Protocol: v1.HTTPProtocolType},
		routeName:        "llm-route",
		routeAnnotations: routeAnnotations,
	}
}

func TestBuildAnnotations(t *testing.T) {
	tests := []struct {
		name       string
		pool       map[string]string
		route      map[string]string
		wantValues map[string]string
		wantAbsent []string
		wantErr    string
	}{
		{
			name: "defaults are imposed when the pool says nothing",
			wantValues: map[string]string{
				lbModeAnnotation:        lbModeFullProxy,
				usePodNetworkAnnotation: "yes",
				"parent-inference-pool": "vllm-qwen3",
				"parent-gateway":        "inference-gw",
				"parent-http-route":     "llm-route",
			},
		},
		{
			name: "pool annotations are carried over",
			pool: map[string]string{epSelectAnnotation: "chwbl", "loxilb.io/model-name": "qwen3-32b"},
			wantValues: map[string]string{
				epSelectAnnotation:      "chwbl",
				"loxilb.io/model-name":  "qwen3-32b",
				lbModeAnnotation:        lbModeFullProxy,
				usePodNetworkAnnotation: "yes",
			},
		},
		{
			// The route is the more specific statement: one pool served by two
			// routes can be tuned per route.
			name:       "route annotations win over the pool",
			pool:       map[string]string{epSelectAnnotation: "chwbl"},
			route:      map[string]string{epSelectAnnotation: "gpuaware"},
			wantValues: map[string]string{epSelectAnnotation: "gpuaware"},
		},
		{
			name:       "non-loxilb annotations are not copied",
			pool:       map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
			wantAbsent: []string{"kubectl.kubernetes.io/last-applied-configuration"},
		},
		{
			name:       "an explicit lbmode is respected",
			pool:       map[string]string{lbModeAnnotation: "fullnat"},
			wantValues: map[string]string{lbModeAnnotation: "fullnat"},
		},
		{
			// Outside fullproxy the kernel selector matches no case, so this
			// combination black-holes traffic instead of routing it.
			name:    "gateway-only selector outside fullproxy is refused",
			pool:    map[string]string{epSelectAnnotation: "chwbl", lbModeAnnotation: "fullnat"},
			wantErr: "requires",
		},
		{
			name:    "usepodnetwork cannot be turned off",
			pool:    map[string]string{usePodNetworkAnnotation: "no"},
			wantErr: usePodNetworkAnnotation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := testManager().buildAnnotations(testPool(tt.pool), testTarget(tt.route))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for key, want := range tt.wantValues {
				if got[key] != want {
					t.Errorf("annotation %s = %q, want %q", key, got[key], want)
				}
			}
			for _, key := range tt.wantAbsent {
				if _, ok := got[key]; ok {
					t.Errorf("annotation %s should not have been copied", key)
				}
			}
		})
	}
}

func TestBuildServices(t *testing.T) {
	pool := testPool(map[string]string{epSelectAnnotation: "chwbl"})
	services, err := testManager().buildServices(pool, []poolTarget{testTarget(nil)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("got %d services, want 1", len(services))
	}

	svc := services[0]
	if svc.Name != "vllm-qwen3-inference" || svc.Namespace != "llm" {
		t.Errorf("name = %s/%s, want llm/vllm-qwen3-inference", svc.Namespace, svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %v, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Spec.LoadBalancerIP != "123.123.123.1" {
		t.Errorf("loadBalancerIP = %q, want the gateway address", svc.Spec.LoadBalancerIP)
	}
	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "loxilb.io/loxilb" {
		t.Errorf("loadBalancerClass = %v, want loxilb.io/loxilb", svc.Spec.LoadBalancerClass)
	}
	if svc.Spec.Selector["app"] != "vllm-qwen3" {
		t.Errorf("selector = %v, want the pool's matchLabels", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 ||
		svc.Spec.Ports[0].Port != 80 ||
		svc.Spec.Ports[0].TargetPort != intstr.FromInt32(8000) {
		t.Errorf("ports = %v, want listener port 80 -> pool targetPort 8000", svc.Spec.Ports)
	}
	if svc.Labels[poolOwnerLabel] != "vllm-qwen3" {
		t.Errorf("owner label = %q, want the pool name", svc.Labels[poolOwnerLabel])
	}
}

func TestBuildServicesRejectsIncompletePool(t *testing.T) {
	tests := []struct {
		name string
		pool *infv1.InferencePool
	}{
		{
			name: "no targetPorts",
			pool: &infv1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "llm"},
				Spec: infv1.InferencePoolSpec{
					Selector: infv1.LabelSelector{MatchLabels: map[infv1.LabelKey]infv1.LabelValue{"app": "x"}},
				},
			},
		},
		{
			name: "no selector",
			pool: &infv1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "llm"},
				Spec:       infv1.InferencePoolSpec{TargetPorts: []infv1.Port{{Number: 8000}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := testManager().buildServices(tt.pool, []poolTarget{testTarget(nil)}); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestServiceNameFor(t *testing.T) {
	target := testTarget(nil)

	if got := serviceNameFor("vllm-qwen3", target, false); got != "vllm-qwen3-inference" {
		t.Errorf("single target name = %q", got)
	}
	if got := serviceNameFor("vllm-qwen3", target, true); got != "vllm-qwen3-inference-inference-gw-http" {
		t.Errorf("multi target name = %q", got)
	}

	// Two long names that share a prefix must not collapse onto one.
	long := strings.Repeat("a", 70)
	first := truncateName(long + "-one")
	second := truncateName(long + "-two")
	if len(first) > maxServiceNameLen || len(second) > maxServiceNameLen {
		t.Errorf("truncated names exceed the limit: %d, %d", len(first), len(second))
	}
	if first == second {
		t.Errorf("distinct names collided after truncation: %q", first)
	}
}

func TestEndpointPickerDecision(t *testing.T) {
	tests := []struct {
		name        string
		ref         infv1.EndpointPickerRef
		wantOK      bool
		wantMessage bool
	}{
		{name: "no picker", ref: infv1.EndpointPickerRef{}, wantOK: true},
		{
			name:        "FailOpen is accepted and reported",
			ref:         infv1.EndpointPickerRef{Name: "epp", FailureMode: infv1.EndpointPickerFailOpen},
			wantOK:      true,
			wantMessage: true,
		},
		{
			name:        "FailClose is refused",
			ref:         infv1.EndpointPickerRef{Name: "epp", FailureMode: infv1.EndpointPickerFailClose},
			wantOK:      false,
			wantMessage: true,
		},
		{
			// FailClose is the API default, so an unset failureMode means the
			// user is asking for the picker to be authoritative.
			name:        "unset failureMode defaults to FailClose",
			ref:         infv1.EndpointPickerRef{Name: "epp"},
			wantOK:      false,
			wantMessage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(nil)
			pool.Spec.EndpointPickerRef = tt.ref

			_, message, ok := endpointPickerDecision(pool)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if (message != "") != tt.wantMessage {
				t.Errorf("message = %q, want message: %v", message, tt.wantMessage)
			}
		})
	}
}

func TestFindListenerForRoute(t *testing.T) {
	gateway := &v1.Gateway{
		Spec: v1.GatewaySpec{Listeners: []v1.Listener{
			{Name: "tcp", Port: 5000, Protocol: v1.TCPProtocolType},
			{Name: "http", Port: 80, Protocol: v1.HTTPProtocolType},
			{Name: "https", Port: 443, Protocol: v1.HTTPSProtocolType},
		}},
	}

	sectionName := v1.SectionName("https")
	port := v1.PortNumber(80)
	missing := v1.SectionName("nope")

	tests := []struct {
		name      string
		parentRef v1.ParentReference
		want      string
	}{
		{name: "by sectionName", parentRef: v1.ParentReference{SectionName: &sectionName}, want: "https"},
		{name: "by port", parentRef: v1.ParentReference{Port: &port}, want: "http"},
		{
			// The extension's own examples leave sectionName out; returning
			// nil here would make them produce nothing at all.
			name:      "falls back to the first HTTP listener",
			parentRef: v1.ParentReference{},
			want:      "http",
		},
		{name: "unknown sectionName matches nothing", parentRef: v1.ParentReference{SectionName: &missing}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findListenerForRoute(gateway, tt.parentRef)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got listener %q, want none", got.Name)
				}
				return
			}
			if got == nil || string(got.Name) != tt.want {
				t.Fatalf("got %v, want listener %q", got, tt.want)
			}
		})
	}
}

func TestPoolsReferencedBy(t *testing.T) {
	poolGroup := v1.Group(inferencePoolGroup)
	poolKind := v1.Kind(inferencePoolKind)
	otherNs := v1.Namespace("other")

	route := &v1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "llm"},
		Spec: v1.HTTPRouteSpec{Rules: []v1.HTTPRouteRule{
			{BackendRefs: []v1.HTTPBackendRef{
				{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{
					Group: &poolGroup, Kind: &poolKind, Name: "pool-a"}}},
				// Same pool twice - one entry.
				{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{
					Group: &poolGroup, Kind: &poolKind, Name: "pool-a"}}},
				// A plain Service is not a pool.
				{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{Name: "nginx"}}},
			}},
			{BackendRefs: []v1.HTTPBackendRef{
				{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{
					Group: &poolGroup, Kind: &poolKind, Name: "pool-b", Namespace: &otherNs}}},
			}},
		}},
	}

	got := poolsReferencedBy(route)
	want := []InferencePoolQueueEntry{
		{Namespace: "llm", Name: "pool-a"},
		{Namespace: "other", Name: "pool-b"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestApplyServiceCreatesUpdatesAndRefusesForeignOwners(t *testing.T) {
	ctx := context.Background()
	pool := testPool(nil)
	m := testManager()

	desired, err := m.buildServices(pool, []poolTarget{testTarget(nil)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := m.applyService(ctx, pool, desired[0]); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// The API server allocates a node port; an update must not drop it, or
	// every reconcile reallocates one and reprograms the rule for nothing.
	created, _ := m.kubeClient.CoreV1().Services("llm").Get(ctx, "vllm-qwen3-inference", metav1.GetOptions{})
	created.Spec.Ports[0].NodePort = 31234
	if _, err := m.kubeClient.CoreV1().Services("llm").Update(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seeding the node port failed: %v", err)
	}

	changed := testPool(map[string]string{epSelectAnnotation: "gpuaware"})
	desired, err = m.buildServices(changed, []poolTarget{testTarget(nil)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := m.applyService(ctx, changed, desired[0]); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	updated, _ := m.kubeClient.CoreV1().Services("llm").Get(ctx, "vllm-qwen3-inference", metav1.GetOptions{})
	if updated.Annotations[epSelectAnnotation] != "gpuaware" {
		t.Errorf("annotation was not updated: %v", updated.Annotations[epSelectAnnotation])
	}
	if updated.Spec.Ports[0].NodePort != 31234 {
		t.Errorf("node port = %d, want it preserved as 31234", updated.Spec.Ports[0].NodePort)
	}

	// A Service someone else owns must not be taken over.
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "vllm-qwen3-inference", Namespace: "other"}}
	if _, err := m.kubeClient.CoreV1().Services("other").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the foreign service failed: %v", err)
	}
	clash := desired[0].DeepCopy()
	clash.Namespace = "other"
	if err := m.applyService(ctx, pool, clash); err == nil {
		t.Error("expected a conflict on a service owned by someone else")
	}
}

func TestDeleteOwnedServicesKeepsWhatIsStillWanted(t *testing.T) {
	ctx := context.Background()
	m := testManager()

	for _, name := range []string{"keep", "drop"} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "llm", Labels: map[string]string{poolOwnerLabel: "vllm-qwen3"},
		}}
		if _, err := m.kubeClient.CoreV1().Services("llm").Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seeding failed: %v", err)
		}
	}
	// Another pool's Service must be left alone.
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "other-pool", Namespace: "llm", Labels: map[string]string{poolOwnerLabel: "another"},
	}}
	if _, err := m.kubeClient.CoreV1().Services("llm").Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	if err := m.deleteOwnedServices(ctx, "llm", "vllm-qwen3", map[string]bool{"keep": true}); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	list, _ := m.kubeClient.CoreV1().Services("llm").List(ctx, metav1.ListOptions{})
	names := map[string]bool{}
	for _, svc := range list.Items {
		names[svc.Name] = true
	}
	if !names["keep"] || !names["other-pool"] || names["drop"] {
		t.Errorf("services after cleanup = %v, want keep and other-pool only", names)
	}
}
