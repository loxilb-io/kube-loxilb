package gatewayapi

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	sigsv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	inffake "sigs.k8s.io/gateway-api-inference-extension/client-go/clientset/versioned/fake"
	infv1lister "sigs.k8s.io/gateway-api-inference-extension/client-go/listers/api/v1"
)

// pickerService - the Service an endpointPickerRef names: a ClusterIP Service
// serving ext_proc on 9002, as the llm-d and GIE charts deploy it.
func pickerService(name string, mutate func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.43.12.7",
			Ports:     []corev1.ServicePort{{Name: "grpc-ext-proc", Port: 9002, Protocol: corev1.ProtocolTCP}},
		},
	}
	if mutate != nil {
		mutate(svc)
	}
	return svc
}

func newIndexer(objs ...interface{}) cache.Indexer {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, obj := range objs {
		_ = indexer.Add(obj)
	}
	return indexer
}

// withServices - point the manager's Service lister at svcs.
func withServices(m *InferencePoolManager, svcs ...*corev1.Service) *InferencePoolManager {
	objs := make([]interface{}, 0, len(svcs))
	for _, svc := range svcs {
		objs = append(objs, svc)
	}
	m.serviceLister = corev1lister.NewServiceLister(newIndexer(objs...))
	return m
}

func pickerRef(name string, mode infv1.EndpointPickerFailureMode) infv1.EndpointPickerRef {
	return infv1.EndpointPickerRef{Name: infv1.ObjectName(name), Port: &infv1.Port{Number: 9002}, FailureMode: mode}
}

func poolWithPicker(annotations map[string]string, ref infv1.EndpointPickerRef) *infv1.InferencePool {
	pool := testPool(annotations)
	pool.Spec.EndpointPickerRef = ref
	return pool
}

func TestResolveEndpointPicker(t *testing.T) {
	group := func(g string) *infv1.Group { v := infv1.Group(g); return &v }

	tests := []struct {
		name         string
		ref          infv1.EndpointPickerRef
		svc          *corev1.Service
		wantDeclared bool
		wantEndpoint string
		wantMode     string
		wantProblem  string
	}{
		{name: "no picker"},
		{
			name:         "resolved",
			ref:          pickerRef("epp", infv1.EndpointPickerFailClose),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantEndpoint: "10.43.12.7:9002", wantMode: eppFailClose,
		},
		{
			// The API default, also what the gateway resolves an empty mode to.
			name:         "unset failureMode is FailClose",
			ref:          pickerRef("epp", ""),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantEndpoint: "10.43.12.7:9002", wantMode: eppFailClose,
		},
		{
			name:         "FailOpen",
			ref:          pickerRef("epp", infv1.EndpointPickerFailOpen),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantEndpoint: "10.43.12.7:9002", wantMode: eppFailOpen,
		},
		{
			name: "explicit core group and Service kind",
			ref: func() infv1.EndpointPickerRef {
				r := pickerRef("epp", infv1.EndpointPickerFailClose)
				r.Group, r.Kind = group(""), "Service"
				return r
			}(),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantEndpoint: "10.43.12.7:9002", wantMode: eppFailClose,
		},
		{
			name: "IPv6 cluster IP",
			ref:  pickerRef("epp", infv1.EndpointPickerFailClose),
			svc: pickerService("epp", func(s *corev1.Service) {
				s.Spec.ClusterIP = "fd00::7"
			}),
			wantDeclared: true, wantEndpoint: "[fd00::7]:9002", wantMode: eppFailClose,
		},
		{
			name: "non-core group",
			ref: func() infv1.EndpointPickerRef {
				r := pickerRef("epp", infv1.EndpointPickerFailClose)
				r.Group = group("example.com")
				return r
			}(),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: `group "example.com"`,
		},
		{
			name: "kind other than Service",
			ref: func() infv1.EndpointPickerRef {
				r := pickerRef("epp", infv1.EndpointPickerFailClose)
				r.Kind = "Pod"
				return r
			}(),
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: `kind "Pod"`,
		},
		{
			name:         "no port",
			ref:          infv1.EndpointPickerRef{Name: "epp", FailureMode: infv1.EndpointPickerFailOpen},
			svc:          pickerService("epp", nil),
			wantDeclared: true, wantMode: eppFailOpen, wantProblem: "without a port",
		},
		{
			name:         "Service not found",
			ref:          pickerRef("epp", infv1.EndpointPickerFailClose),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: "llm/epp not found",
		},
		{
			name: "ExternalName Service",
			ref:  pickerRef("epp", infv1.EndpointPickerFailClose),
			svc: pickerService("epp", func(s *corev1.Service) {
				s.Spec.Type = corev1.ServiceTypeExternalName
				s.Spec.ClusterIP = ""
				s.Spec.ExternalName = "epp.example.com"
			}),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: "ExternalName",
		},
		{
			name: "headless Service",
			ref:  pickerRef("epp", infv1.EndpointPickerFailClose),
			svc: pickerService("epp", func(s *corev1.Service) {
				s.Spec.ClusterIP = corev1.ClusterIPNone
			}),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: "headless",
		},
		{
			// endpointPickerRef names the Service port, not the target port.
			name: "port the Service does not expose",
			ref:  pickerRef("epp", infv1.EndpointPickerFailClose),
			svc: pickerService("epp", func(s *corev1.Service) {
				s.Spec.Ports[0].Port = 9003
			}),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: "has no port 9002",
		},
		{
			name: "UDP port only",
			ref:  pickerRef("epp", infv1.EndpointPickerFailClose),
			svc: pickerService("epp", func(s *corev1.Service) {
				s.Spec.Ports[0].Protocol = corev1.ProtocolUDP
			}),
			wantDeclared: true, wantMode: eppFailClose, wantProblem: "has no port 9002",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManager()
			if tt.svc != nil {
				withServices(m, tt.svc)
			} else {
				withServices(m)
			}

			got, err := m.resolveEndpointPicker(poolWithPicker(nil, tt.ref))
			if err != nil {
				t.Fatalf("resolveEndpointPicker: %v", err)
			}

			if got.declared != tt.wantDeclared {
				t.Errorf("declared = %v, want %v", got.declared, tt.wantDeclared)
			}
			if got.endpoint != tt.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", got.endpoint, tt.wantEndpoint)
			}
			if got.failureMode != tt.wantMode {
				t.Errorf("failureMode = %q, want %q", got.failureMode, tt.wantMode)
			}
			if tt.wantProblem == "" && got.problem != "" {
				t.Errorf("problem = %q, want none", got.problem)
			}
			if tt.wantProblem != "" && !strings.Contains(got.problem, tt.wantProblem) {
				t.Errorf("problem = %q, want it to contain %q", got.problem, tt.wantProblem)
			}
		})
	}
}

func TestPickerReport(t *testing.T) {
	resolved := pickerResolution{declared: true, name: "epp", endpoint: "10.43.12.7:9002", failureMode: eppFailClose}
	openGone := pickerResolution{declared: true, name: "epp", failureMode: eppFailOpen, problem: "endpoint picker Service llm/epp not found"}
	closedGone := openGone
	closedGone.failureMode = eppFailClose

	tests := []struct {
		name        string
		picker      pickerResolution
		wantOK      bool
		wantReason  string
		wantMessage string
	}{
		{"no picker", pickerResolution{}, true, string(infv1.InferencePoolReasonResolvedRefs), ""},
		{"resolved", resolved, true, string(infv1.InferencePoolReasonResolvedRefs), "resolved to 10.43.12.7:9002"},
		{"unresolved FailOpen", openGone, false, string(infv1.InferencePoolReasonInvalidExtensionRef), "served by loxilb's own endpoint selection"},
		{"unresolved FailClose", closedGone, false, string(infv1.InferencePoolReasonInvalidExtensionRef), "answered 503 until it resolves"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.picker.report(true, string(infv1.InferencePoolReasonAccepted), "managed by kube-loxilb")

			if r.resolvedRefs != tt.wantOK || r.resolvedRefsReason != tt.wantReason {
				t.Errorf("ResolvedRefs = %v/%s, want %v/%s", r.resolvedRefs, r.resolvedRefsReason, tt.wantOK, tt.wantReason)
			}
			if !strings.Contains(r.resolvedRefsMessage, tt.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", r.resolvedRefsMessage, tt.wantMessage)
			}
			if !r.accepted || r.acceptedReason != string(infv1.InferencePoolReasonAccepted) {
				t.Errorf("Accepted = %v/%s, want what the caller passed", r.accepted, r.acceptedReason)
			}
		})
	}
}

func TestBuildAnnotationsEndpointPicker(t *testing.T) {
	resolved := pickerResolution{declared: true, name: "epp", endpoint: "10.43.12.7:9002", failureMode: eppFailOpen}
	unresolved := pickerResolution{declared: true, name: "epp", failureMode: eppFailOpen, problem: "not found"}
	unresolvedClosed := pickerResolution{declared: true, name: "epp", failureMode: eppFailClose, problem: "not found"}

	tests := []struct {
		name       string
		pool       map[string]string
		route      map[string]string
		picker     pickerResolution
		wantValues map[string]string
		wantAbsent []string
		wantErr    string
	}{
		{
			name:   "a resolved picker is written from the spec",
			picker: resolved,
			wantValues: map[string]string{
				eppEndpointAnnotation:    "10.43.12.7:9002",
				eppFailureModeAnnotation: eppFailOpen,
				lbModeAnnotation:         lbModeFullProxy,
			},
		},
		{
			name: "a user value cannot replace the spec's picker",
			pool: map[string]string{
				eppEndpointAnnotation:    "1.2.3.4:9",
				eppFailureModeAnnotation: eppFailClose,
			},
			picker: resolved,
			wantValues: map[string]string{
				eppEndpointAnnotation:    "10.43.12.7:9002",
				eppFailureModeAnnotation: eppFailOpen,
			},
		},
		{
			name:       "a user value cannot switch on a picker the spec does not name",
			pool:       map[string]string{eppEndpointAnnotation: "1.2.3.4:9", eppFailureModeAnnotation: eppFailClose},
			route:      map[string]string{eppEndpointAnnotation: "5.6.7.8:9"},
			wantAbsent: []string{eppEndpointAnnotation, eppFailureModeAnnotation},
		},
		{
			name:       "an unresolved FailOpen picker writes nothing",
			pool:       map[string]string{eppEndpointAnnotation: "1.2.3.4:9"},
			picker:     unresolved,
			wantAbsent: []string{eppEndpointAnnotation, eppFailureModeAnnotation},
		},
		{
			// Kept off the model servers, but answered: the gateway gets an
			// address that always refuses and replies 503.
			name:   "an unresolved FailClose picker points the rule at a refusing address",
			pool:   map[string]string{eppEndpointAnnotation: "1.2.3.4:9"},
			picker: unresolvedClosed,
			wantValues: map[string]string{
				eppEndpointAnnotation:    eppRefusedEndpoint,
				eppFailureModeAnnotation: eppFailClose,
			},
		},
		{
			// Still an EPP rule to the gateway, which would refuse it outside
			// fullproxy.
			name:    "the refusing address is held to the same combination checks",
			pool:    map[string]string{lbModeAnnotation: "fullnat"},
			picker:  unresolvedClosed,
			wantErr: "an endpoint picker requires loxilb.io/lbmode",
		},
		{
			// Neither has a field in the API, so they travel as annotations,
			// the route tuning the pool like any other loxilb setting.
			name:   "deadline and transport come from the pool and the route",
			pool:   map[string]string{"loxilb.io/epp-timeout-ms": "5000", "loxilb.io/epp-plaintext": "true"},
			route:  map[string]string{"loxilb.io/epp-timeout-ms": "8000"},
			picker: resolved,
			wantValues: map[string]string{
				"loxilb.io/epp-timeout-ms": "8000",
				"loxilb.io/epp-plaintext":  "true",
			},
		},
		{
			name:    "a picker outside fullproxy is refused",
			pool:    map[string]string{lbModeAnnotation: "fullnat"},
			picker:  resolved,
			wantErr: "an endpoint picker requires loxilb.io/lbmode",
		},
		{
			name:    "a picker with loxilb P/D is refused",
			pool:    map[string]string{pdDisaggAnnotation: "yes"},
			picker:  resolved,
			wantErr: "cannot be combined with loxilb.io/pd-disagg",
		},
		{
			// Without a picker the P/D annotation is the loadbalancer
			// manager's to judge, as before.
			name:       "P/D without a picker is not this check's business",
			pool:       map[string]string{pdDisaggAnnotation: "yes"},
			wantValues: map[string]string{pdDisaggAnnotation: "yes"},
		},
		{
			name:       "an unresolved FailOpen picker does not trip the fullproxy check",
			pool:       map[string]string{lbModeAnnotation: "fullnat"},
			picker:     unresolved,
			wantValues: map[string]string{lbModeAnnotation: "fullnat"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := testManager().buildAnnotations(testPool(tt.pool), testTarget(tt.route), tt.picker)
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
				if v, ok := got[key]; ok {
					t.Errorf("annotation %s = %q, want it absent", key, v)
				}
			}
		})
	}
}

func TestBuildServicesEndpointPickerNeedsOneTargetPort(t *testing.T) {
	pool := testPool(nil)
	pool.Spec.TargetPorts = []infv1.Port{{Number: 8000}, {Number: 8001}}
	resolved := pickerResolution{declared: true, name: "epp", endpoint: "10.43.12.7:9002", failureMode: eppFailClose}

	if _, err := testManager().buildServices(pool, []poolTarget{testTarget(nil)}, resolved); err == nil ||
		!strings.Contains(err.Error(), "declare a single targetPort") {
		t.Errorf("buildServices = %v, want a refusal naming the single-targetPort requirement", err)
	}

	unresolvedClosed := pickerResolution{declared: true, name: "epp", failureMode: eppFailClose, problem: "not found"}
	if _, err := testManager().buildServices(pool, []poolTarget{testTarget(nil)}, unresolvedClosed); err == nil {
		t.Error("buildServices with an unresolved FailClose picker = nil, want the same refusal: the rule still carries an EPP")
	}

	// Without a picker the first port is served and the rest are warned about,
	// as before.
	if _, err := testManager().buildServices(pool, []poolTarget{testTarget(nil)}, pickerResolution{}); err != nil {
		t.Errorf("buildServices without a picker = %v, want nil", err)
	}
}

func TestEnqueuePickerPools(t *testing.T) {
	pools := []interface{}{
		poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose)),
		func() *infv1.InferencePool {
			p := poolWithPicker(nil, pickerRef("other-epp", infv1.EndpointPickerFailClose))
			p.Name = "other"
			return p
		}(),
		func() *infv1.InferencePool { p := testPool(nil); p.Name = "plain"; return p }(),
		func() *infv1.InferencePool {
			p := poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose))
			p.Namespace = "elsewhere"
			return p
		}(),
	}

	newManager := func() *InferencePoolManager {
		m := testManager()
		m.poolLister = infv1lister.NewInferencePoolLister(newIndexer(pools...))
		m.queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[InferencePoolQueueEntry]())
		return m
	}

	t.Run("the pools naming the Service in its namespace", func(t *testing.T) {
		m := newManager()
		m.enqueuePickerPools(pickerService("epp", nil))

		if m.queue.Len() != 1 {
			t.Fatalf("queued %d pools, want 1", m.queue.Len())
		}
		if got, _ := m.queue.Get(); got != (InferencePoolQueueEntry{Namespace: "llm", Name: "vllm-qwen3"}) {
			t.Errorf("queued %v, want llm/vllm-qwen3", got)
		}
	})

	t.Run("a deleted Service seen only as a tombstone", func(t *testing.T) {
		m := newManager()
		m.enqueuePickerPools(cache.DeletedFinalStateUnknown{Key: "llm/epp", Obj: pickerService("epp", nil)})

		if m.queue.Len() != 1 {
			t.Errorf("queued %d pools, want 1", m.queue.Len())
		}
	})

	t.Run("a Service this manager made", func(t *testing.T) {
		m := newManager()
		m.enqueuePickerPools(pickerService("epp", func(s *corev1.Service) {
			s.Labels = map[string]string{poolOwnerLabel: "vllm-qwen3"}
		}))

		if m.queue.Len() != 0 {
			t.Errorf("queued %d pools for a generated Service, want 0", m.queue.Len())
		}
	})
}

func TestPickerAddressChanged(t *testing.T) {
	base := pickerService("epp", nil)

	tests := []struct {
		name   string
		mutate func(*corev1.Service)
		want   bool
	}{
		{"metadata only", func(s *corev1.Service) { s.Annotations = map[string]string{"x": "y"} }, false},
		{"cluster IP", func(s *corev1.Service) { s.Spec.ClusterIP = "10.43.99.9" }, true},
		{"ports", func(s *corev1.Service) { s.Spec.Ports[0].Port = 9003 }, true},
		{"type", func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickerAddressChanged(base, pickerService("epp", tt.mutate)); got != tt.want {
				t.Errorf("pickerAddressChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

// reconcileManager - a manager wired to fakes: one Gateway with an HTTP
// listener and an address, one HTTPRoute sending to pool, and svcs in the
// Service cache.
func reconcileManager(pool *infv1.InferencePool, svcs ...*corev1.Service) *InferencePoolManager {
	gw := &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "inference-gw", Namespace: "llm"},
		Spec: v1.GatewaySpec{
			GatewayClassName: "loxilb.io/loxilb",
			Listeners:        []v1.Listener{{Name: "http", Port: 80, Protocol: v1.HTTPProtocolType}},
		},
		Status: v1.GatewayStatus{Addresses: []v1.GatewayStatusAddress{{Value: "123.123.123.1"}}},
	}
	group := v1.Group(inferencePoolGroup)
	kind := v1.Kind(inferencePoolKind)
	route := &v1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-route", Namespace: "llm"},
		Spec: v1.HTTPRouteSpec{
			CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: []v1.ParentReference{{Name: "inference-gw"}}},
			Rules: []v1.HTTPRouteRule{{BackendRefs: []v1.HTTPBackendRef{{BackendRef: v1.BackendRef{
				BackendObjectReference: v1.BackendObjectReference{Group: &group, Kind: &kind, Name: v1.ObjectName(pool.Name)},
			}}}}},
		},
	}

	// Created through the typed v1 client rather than handed to the
	// constructor: v1beta1.Gateway aliases the v1 type, so the constructor's
	// tracker may file the object under v1beta1, where a v1 Get misses it.
	sigsClient := sigsfake.NewSimpleClientset()
	if _, err := sigsClient.GatewayV1().Gateways("llm").Create(context.Background(), gw, metav1.CreateOptions{}); err != nil {
		panic(err)
	}

	m := testManager()
	m.sigsClient = sigsClient
	m.infClient = inffake.NewSimpleClientset(pool)
	m.poolLister = infv1lister.NewInferencePoolLister(newIndexer(pool))
	m.httpRouteLister = sigsv1lister.NewHTTPRouteLister(newIndexer(route))
	return withServices(m, svcs...)
}

func ownedServices(t *testing.T, m *InferencePoolManager) []corev1.Service {
	t.Helper()

	list, err := m.kubeClient.CoreV1().Services("llm").List(context.Background(),
		metav1.ListOptions{LabelSelector: poolOwnerLabel + "=vllm-qwen3"})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	return list.Items
}

func poolCondition(t *testing.T, m *InferencePoolManager, conditionType string) metav1.Condition {
	t.Helper()

	pool, err := m.infClient.InferenceV1().InferencePools("llm").Get(context.Background(), "vllm-qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	for _, parent := range pool.Status.Parents {
		if parent.ControllerName != inferencePoolControllerName {
			continue
		}
		for _, c := range parent.Conditions {
			if c.Type == conditionType {
				return c
			}
		}
	}
	t.Fatalf("pool has no %s condition from %s: %+v", conditionType, inferencePoolControllerName, pool.Status.Parents)
	return metav1.Condition{}
}

// The status matrix of the plan, row by row, through reconcile.
func TestReconcileEndpointPicker(t *testing.T) {
	tests := []struct {
		name             string
		pool             *infv1.InferencePool
		svcs             []*corev1.Service
		wantServed       bool
		wantEndpoint     string
		wantAccepted     metav1.ConditionStatus
		wantAcceptReason string
		wantResolved     metav1.ConditionStatus
		wantRefReason    string
	}{
		{
			name:         "no picker",
			pool:         testPool(nil),
			wantServed:   true,
			wantAccepted: metav1.ConditionTrue, wantAcceptReason: string(infv1.InferencePoolReasonAccepted),
			wantResolved: metav1.ConditionTrue, wantRefReason: string(infv1.InferencePoolReasonResolvedRefs),
		},
		{
			name:         "resolved picker",
			pool:         poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose)),
			svcs:         []*corev1.Service{pickerService("epp", nil)},
			wantServed:   true,
			wantEndpoint: "10.43.12.7:9002",
			wantAccepted: metav1.ConditionTrue, wantAcceptReason: string(infv1.InferencePoolReasonAccepted),
			wantResolved: metav1.ConditionTrue, wantRefReason: string(infv1.InferencePoolReasonResolvedRefs),
		},
		{
			name:         "unresolved FailOpen is served without the picker",
			pool:         poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailOpen)),
			wantServed:   true,
			wantAccepted: metav1.ConditionTrue, wantAcceptReason: string(infv1.InferencePoolReasonAccepted),
			wantResolved: metav1.ConditionFalse, wantRefReason: string(infv1.InferencePoolReasonInvalidExtensionRef),
		},
		{
			// The rule stays so requests get a 503 from the gateway rather than
			// a refused connection; it points at an address that always refuses.
			name:         "unresolved FailClose is answered 503 by the gateway",
			pool:         poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose)),
			wantServed:   true,
			wantEndpoint: eppRefusedEndpoint,
			wantAccepted: metav1.ConditionTrue, wantAcceptReason: string(infv1.InferencePoolReasonAccepted),
			wantResolved: metav1.ConditionFalse, wantRefReason: string(infv1.InferencePoolReasonInvalidExtensionRef),
		},
		{
			name:         "picker combined with loxilb P/D",
			pool:         poolWithPicker(map[string]string{pdDisaggAnnotation: "true"}, pickerRef("epp", infv1.EndpointPickerFailOpen)),
			svcs:         []*corev1.Service{pickerService("epp", nil)},
			wantServed:   false,
			wantAccepted: metav1.ConditionFalse, wantAcceptReason: string(infv1.InferencePoolReasonNotSupportedByParent),
			wantResolved: metav1.ConditionTrue, wantRefReason: string(infv1.InferencePoolReasonResolvedRefs),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := reconcileManager(tt.pool, tt.svcs...)

			if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			services := ownedServices(t, m)
			if served := len(services) > 0; served != tt.wantServed {
				t.Fatalf("served = %v (%d services), want %v", served, len(services), tt.wantServed)
			}
			if tt.wantServed {
				if got := services[0].Annotations[eppEndpointAnnotation]; got != tt.wantEndpoint {
					t.Errorf("%s = %q, want %q", eppEndpointAnnotation, got, tt.wantEndpoint)
				}
			}

			accepted := poolCondition(t, m, string(infv1.InferencePoolConditionAccepted))
			if accepted.Status != tt.wantAccepted || accepted.Reason != tt.wantAcceptReason {
				t.Errorf("Accepted = %s/%s, want %s/%s", accepted.Status, accepted.Reason, tt.wantAccepted, tt.wantAcceptReason)
			}
			resolved := poolCondition(t, m, string(infv1.InferencePoolConditionResolvedRefs))
			if resolved.Status != tt.wantResolved || resolved.Reason != tt.wantRefReason {
				t.Errorf("ResolvedRefs = %s/%s (%s), want %s/%s",
					resolved.Status, resolved.Reason, resolved.Message, tt.wantResolved, tt.wantRefReason)
			}
		})
	}
}

// A FailClose pool whose picker Service goes away keeps its rule but points it
// at the refusing address, and moves to the new address once the Service is
// back. The Service and its VIP stay throughout.
func TestReconcileFailClosePickerComesAndGoes(t *testing.T) {
	pool := poolWithPicker(nil, pickerRef("epp", infv1.EndpointPickerFailClose))
	m := reconcileManager(pool, pickerService("epp", nil))

	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if services := ownedServices(t, m); len(services) != 1 {
		t.Fatalf("got %d services with the picker present, want 1", len(services))
	}

	withServices(m)
	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile without the picker: %v", err)
	}
	services := ownedServices(t, m)
	if len(services) != 1 {
		t.Fatalf("got %d services after the picker went away, want 1", len(services))
	}
	if got := services[0].Annotations[eppEndpointAnnotation]; got != eppRefusedEndpoint {
		t.Errorf("%s = %q with the picker gone, want the refusing address", eppEndpointAnnotation, got)
	}

	withServices(m, pickerService("epp", func(s *corev1.Service) { s.Spec.ClusterIP = "10.43.99.9" }))
	if err := m.reconcile("llm", "vllm-qwen3"); err != nil {
		t.Fatalf("reconcile with the picker back: %v", err)
	}
	services = ownedServices(t, m)
	if len(services) != 1 {
		t.Fatalf("got %d services after the picker came back, want 1", len(services))
	}
	if got := services[0].Annotations[eppEndpointAnnotation]; got != "10.43.99.9:9002" {
		t.Errorf("%s = %q, want the new cluster IP", eppEndpointAnnotation, got)
	}
}
