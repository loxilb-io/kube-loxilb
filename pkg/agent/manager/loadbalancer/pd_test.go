package loadbalancer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func pdPod(name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Status: corev1.PodStatus{
			PodIP:  ip,
			PodIPs: []corev1.PodIP{{IP: ip}},
		},
	}
}

// pdManager builds a Manager backed by a fake API server holding pods.
func pdManager(pods ...*corev1.Pod) *Manager {
	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	return &Manager{
		kubeClient:    k8sfake.NewSimpleClientset(objs...),
		networkConfig: &config.NetworkConfig{SetLBMode: uint16(api.LBModeFullProxy)},
	}
}

func pdAnnotations() map[string]string {
	return map[string]string{
		lbModeAnnotation:            "fullproxy",
		usePodNetworkAnnotation:     "yes",
		pdDisaggAnnotation:          "true",
		pdPrefillSelectorAnnotation: "llm-role=prefill",
		pdDecodeSelectorAnnotation:  "llm-role=decode",
		pdPrefillNixlPortAnnotation: "9001",
		pdDecodeNixlPortAnnotation:  "9002",
		sseModeAnnotation:           "true",
	}
}

func TestResolvePDRoles(t *testing.T) {
	m := pdManager(
		pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-prefill-1", "10.244.1.11", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-decode-0", "10.244.2.10", map[string]string{"llm-role": "decode"}),
		pdPod("unrelated", "10.244.3.10", map[string]string{"app": "nginx"}),
	)
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if !aiArgs.PDDisaggMode {
		t.Fatal("PDDisaggMode was not parsed")
	}

	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	want := map[string]pdEndpointRole{
		"10.244.1.10": {Role: api.EpRolePrefill, NixlPort: 9001},
		"10.244.1.11": {Role: api.EpRolePrefill, NixlPort: 9001},
		"10.244.2.10": {Role: api.EpRoleDecode, NixlPort: 9002},
	}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for ip, w := range want {
		if roles[ip] != w {
			t.Errorf("roles[%s] = %+v, want %+v", ip, roles[ip], w)
		}
	}
	if _, matched := roles["10.244.3.10"]; matched {
		t.Error("a pod matching neither selector was given a role")
	}
}

// A pod in both pools would make the rule depend on map iteration order.
func TestResolvePDRolesRejectsOverlappingSelectors(t *testing.T) {
	m := pdManager(
		pdPod("both", "10.244.1.10", map[string]string{"llm-role": "prefill", "tier": "gpu"}),
		pdPod("decode-0", "10.244.2.10", map[string]string{"llm-role": "decode", "tier": "gpu"}),
	)
	annotations := pdAnnotations()
	annotations[pdDecodeSelectorAnnotation] = "tier=gpu"
	svc := aiSvc(annotations)

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}

	_, err = m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err == nil {
		t.Fatal("overlapping selectors were accepted")
	}
	if !strings.Contains(err.Error(), "matches both") {
		t.Errorf("error = %q, want it to name the overlap", err)
	}
}

// Without disaggregation nothing is resolved and no endpoint gains a role.
func TestResolvePDRolesInertWithoutDisagg(t *testing.T) {
	m := pdManager(pdPod("p", "10.244.1.10", map[string]string{"llm-role": "prefill"}))

	roles, err := m.resolvePDRoles(context.Background(), aiSvc(nil), api.AIArgs{SseMode: true})
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}
	if roles != nil {
		t.Errorf("roles = %v, want nil", roles)
	}
}

func TestGetAIArgsPDRejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		lbMode  int
		wantErr string
	}{
		{
			name: "no selectors",
			mutate: func(a map[string]string) {
				delete(a, pdPrefillSelectorAnnotation)
				delete(a, pdDecodeSelectorAnnotation)
			},
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "requires both loxilb.io/pd-prefill-selector and loxilb.io/pd-decode-selector",
		},
		{
			name:    "only the prefill selector",
			mutate:  func(a map[string]string) { delete(a, pdDecodeSelectorAnnotation) },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "requires both",
		},
		{
			name:    "outside fullproxy",
			mutate:  func(a map[string]string) {},
			lbMode:  int(api.LBModeFullNat),
			wantErr: "pd-disagg requires mode=fullproxy",
		},
		{
			name:    "malformed selector",
			mutate:  func(a map[string]string) { a[pdPrefillSelectorAnnotation] = "llm-role==!!" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "is not a label selector",
		},
		{
			name:    "nixl port out of range",
			mutate:  func(a map[string]string) { a[pdPrefillNixlPortAnnotation] = "70000" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "loxilb.io/pd-prefill-nixl-port must be within 0..65535",
		},
		{
			name:    "cache-aware without disagg",
			mutate:  func(a map[string]string) { delete(a, pdDisaggAnnotation); a[pdCacheAwareAnnotation] = "true" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "pd-cache-aware requires pd_disagg_mode=true",
		},
		{
			name:    "kv-exact single-role contradicts disagg",
			mutate:  func(a map[string]string) { a[kvExactModeAnnotation] = "3" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "incompatible with pd-disagg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := pdAnnotations()
			tt.mutate(annotations)
			m := pdManager()

			_, err := m.getAIArgs(aiSvc(annotations), tt.lbMode, api.LbSelRr)
			if err == nil {
				t.Fatalf("getAIArgs = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("getAIArgs = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// kvExactMode=1 is only legal alongside disaggregation, and now reachable.
func TestGetAIArgsKvExactZmqWithDisagg(t *testing.T) {
	annotations := pdAnnotations()
	annotations[kvExactModeAnnotation] = "1"
	m := pdManager()

	got, err := m.getAIArgs(aiSvc(annotations), int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if got.KvExactMode != api.KvExactModeZmq || !got.PDDisaggMode {
		t.Errorf("got kvExactMode=%d pdDisagg=%v, want 1/true", got.KvExactMode, got.PDDisaggMode)
	}
}

// The assembled payload must match the shape loxilb's own P/D scenario posts:
// pd_disagg_mode on the service, and ep_role plus nixl_port on every endpoint.
func TestPDPayloadMatchesGatewayScenario(t *testing.T) {
	m := pdManager(
		pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-decode-0", "10.244.2.10", map[string]string{"llm-role": "decode"}),
	)
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	// mirrors how makeLoxiLoadBalancerModel stamps the endpoints
	var eps []api.LoadBalancerEndpoint
	for _, ip := range []string{"10.244.1.10", "10.244.2.10"} {
		r := roles[ip]
		eps = append(eps, api.LoadBalancerEndpoint{
			EndpointIP: ip, TargetPort: 8000, Weight: 1,
			EpRole: r.Role, NixlPort: r.NixlPort,
		})
	}

	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "20.20.20.1", Port: 2020, Protocol: "tcp",
			Mode: api.LBModeFullProxy, AIArgs: aiArgs,
		},
		Endpoints: eps,
	}

	if err := lb.Service.AIArgs.Validate(lb.Service.Mode, lb.Endpoints); err != nil {
		t.Fatalf("assembled payload fails validation: %v", err)
	}

	body, err := json.Marshal(&lb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(body)

	for _, want := range []string{
		`"pd_disagg_mode":true`, `"sse_mode":true`, `"mode":4`,
		`"ep_role":1`, `"nixl_port":9001`,
		`"ep_role":2`, `"nixl_port":9002`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s\n%s", want, payload)
		}
	}
}

// A disaggregated rule whose decode pool has no ready pods must be refused
// before it reaches loxilb.
func TestPDPayloadRejectedWhenAPoolIsEmpty(t *testing.T) {
	m := pdManager(pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}))
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	r := roles["10.244.1.10"]
	eps := []api.LoadBalancerEndpoint{
		{EndpointIP: "10.244.1.10", TargetPort: 8000, Weight: 1, EpRole: r.Role, NixlPort: r.NixlPort},
	}

	err = aiArgs.Validate(api.LBModeFullProxy, eps)
	if err == nil {
		t.Fatal("a prefill-only disaggregated rule was accepted")
	}
	if !strings.Contains(err.Error(), "at least 1 prefill (ep_role=1) and 1 decode (ep_role=2)") {
		t.Errorf("error = %q, want loxilb's own wording", err)
	}
}
