package loadbalancer

import (
	"strings"
	"testing"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func aiSvc(annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "vllm",
			Namespace:   "default",
			Annotations: annotations,
		},
	}
}

// managerWithMode builds a Manager whose agent-wide default LB mode is mode.
func managerWithMode(mode uint16) *Manager {
	return &Manager{networkConfig: &config.NetworkConfig{SetLBMode: mode}}
}

// A service with no inference annotations must produce a zero AIArgs, which is
// what keeps its payload identical to a plain loxilb rule.
func TestGetAIArgsNoAnnotations(t *testing.T) {
	m := managerWithMode(0)

	got, err := m.getAIArgs(aiSvc(map[string]string{"loxilb.io/lbmode": "fullproxy"}), api.LBModeFullProxy, api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if got.IsSet() {
		t.Errorf("AIArgs = %+v, want zero", got)
	}
}

// CHWBL prefix-cache routing: use case 1.
func TestGetAIArgsCHWBL(t *testing.T) {
	m := managerWithMode(0)
	svc := aiSvc(map[string]string{
		endPointSelAnnotation:          "chwbl",
		chwblPrefixHashLevelAnnotation: "2",
		chwblMeanLoadFactorAnnotation:  "125",
		chwblReplicationAnnotation:     "100",
		chwblEnableCacheSaltAnnotation: "true",
		sseModeAnnotation:              "yes",
		modelNameAnnotation:            "llama-70b",
	})

	got, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelCHWBL)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}

	if got.ChwblPrefixHashLevel != 2 {
		t.Errorf("ChwblPrefixHashLevel = %d, want 2", got.ChwblPrefixHashLevel)
	}
	if got.ChwblMeanLoadFactor != 125 {
		t.Errorf("ChwblMeanLoadFactor = %d, want 125", got.ChwblMeanLoadFactor)
	}
	if !got.ChwblEnableCacheSalt {
		t.Error("ChwblEnableCacheSalt = false, want true")
	}
	if !got.SseMode {
		t.Error("SseMode = false, want true (yes must parse)")
	}
	if got.ModelName != "llama-70b" {
		t.Errorf("ModelName = %q, want llama-70b", got.ModelName)
	}
}

// Single role-less pool KV-exact routing: use case 3 (SGLang).
func TestGetAIArgsSingleRoleKVExact(t *testing.T) {
	m := managerWithMode(0)
	svc := aiSvc(map[string]string{
		kvExactModeAnnotation:   "3",
		kvEngineTypeAnnotation:  "sglang",
		kvDpRankCountAnnotation: "4",
		kvBlockSizeAnnotation:   "16",
	})

	got, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}

	if got.KvExactMode != api.KvExactModeSingleRole {
		t.Errorf("KvExactMode = %d, want 3", got.KvExactMode)
	}
	if got.KvEngineType != "sglang" || got.KvDpRankCount != 4 {
		t.Errorf("engine/dp = %q/%d, want sglang/4", got.KvEngineType, got.KvDpRankCount)
	}
	// omitted on purpose: the server derives it from the engine
	if got.KvHashAlgo != "" {
		t.Errorf("KvHashAlgo = %q, want empty", got.KvHashAlgo)
	}
}

func TestGetAIArgsRejections(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		lbMode      int
		sel         api.EpSelect
		defaultMode uint16
		wantErr     string
	}{
		{
			name:        "chwbl outside fullproxy",
			annotations: map[string]string{endPointSelAnnotation: "chwbl"},
			lbMode:      int(api.LBModeDefault),
			sel:         api.LbSelCHWBL,
			wantErr:     "requires loxilb.io/lbmode=fullproxy",
		},
		{
			name:        "chwbl inherits a fullproxy agent default",
			annotations: map[string]string{endPointSelAnnotation: "chwbl"},
			lbMode:      -1,
			sel:         api.LbSelCHWBL,
			defaultMode: uint16(api.LBModeFullProxy),
		},
		{
			name:        "chwbl with a non-fullproxy agent default",
			annotations: map[string]string{endPointSelAnnotation: "chwbl"},
			lbMode:      -1,
			sel:         api.LbSelCHWBL,
			defaultMode: uint16(api.LBModeDefault),
			wantErr:     "requires loxilb.io/lbmode=fullproxy",
		},
		{
			name:        "non-integer value",
			annotations: map[string]string{kvZmqPortAnnotation: "abc"},
			lbMode:      int(api.LBModeFullProxy),
			sel:         api.LbSelRr,
			wantErr:     `loxilb.io/kv-zmq-port: "abc" is not an integer`,
		},
		{
			name:        "non-boolean value",
			annotations: map[string]string{sseModeAnnotation: "maybe"},
			lbMode:      int(api.LBModeFullProxy),
			sel:         api.LbSelRr,
			wantErr:     `loxilb.io/sse-mode: "maybe" is not a boolean`,
		},
		{
			name:        "kv-exact mode 1 needs pd-disagg, which annotations cannot express",
			annotations: map[string]string{kvExactModeAnnotation: "1"},
			lbMode:      int(api.LBModeFullProxy),
			sel:         api.LbSelRr,
			wantErr:     "use kvExactMode=3 for a single pool",
		},
		{
			name:        "kv-exact single role outside fullproxy",
			annotations: map[string]string{kvExactModeAnnotation: "3"},
			lbMode:      int(api.LBModeDefault),
			sel:         api.LbSelRr,
			wantErr:     "kv-exact single-role mode requires mode=fullproxy",
		},
		{
			name: "hash algo incoherent with engine",
			annotations: map[string]string{
				kvEngineTypeAnnotation: "sglang",
				kvHashAlgoAnnotation:   "sha256_cbor",
			},
			lbMode:  int(api.LBModeFullProxy),
			sel:     api.LbSelRr,
			wantErr: "is incompatible with kv-engine-type",
		},
		{
			name:        "chwbl mean load factor out of range",
			annotations: map[string]string{chwblMeanLoadFactorAnnotation: "500"},
			lbMode:      int(api.LBModeFullProxy),
			sel:         api.LbSelRr,
			wantErr:     "chwbl_mean_load_factor must be within 100..300",
		},
		{
			name:        "unknown engine",
			annotations: map[string]string{kvEngineTypeAnnotation: "trtllm"},
			lbMode:      int(api.LBModeFullProxy),
			sel:         api.LbSelRr,
			wantErr:     "kv-engine-type must be one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managerWithMode(tt.defaultMode)

			got, err := m.getAIArgs(aiSvc(tt.annotations), tt.lbMode, tt.sel)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("getAIArgs = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("getAIArgs = nil, want error containing %q (got %+v)", tt.wantErr, got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("getAIArgs = %q, want it to contain %q", err, tt.wantErr)
			}
			if got.IsSet() {
				t.Errorf("a rejected config still returned args: %+v", got)
			}
		})
	}
}

// P/D disaggregation must not be reachable from annotations: it needs a
// per-endpoint role that a Service cannot express.
func TestPDDisaggNotExposedViaAnnotations(t *testing.T) {
	m := managerWithMode(uint16(api.LBModeFullProxy))
	svc := aiSvc(map[string]string{
		"loxilb.io/pd-disagg-mode": "true",
		"loxilb.io/ep-role":        "prefill",
	})

	got, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if got.PDDisaggMode {
		t.Error("pd_disagg_mode became settable from an annotation")
	}
	if got.IsSet() {
		t.Errorf("unrecognised annotations produced args: %+v", got)
	}
}

// A gateway-only selector must be refused on plain loxilb even when no AI
// serviceArguments accompany it.
func TestInstallLBRefusesGatewaySelectorOnPlainLoxilb(t *testing.T) {
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			Mode:       api.LBModeFullProxy,
			Sel:        api.LbSelCHWBL,
		},
	}

	err := m.installLB(plain.client(t), lb, false)
	if err == nil {
		t.Fatal("installLB accepted sel=chwbl on plain loxilb")
	}
	if !errors.Is(err, ErrInferenceGatewayRequired) {
		t.Errorf("error %q does not wrap ErrInferenceGatewayRequired", err)
	}
}

// The same selector must go through to a gateway untouched.
func TestInstallLBAllowsGatewaySelectorOnGateway(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			Mode:       api.LBModeFullProxy,
			Sel:        api.LbSelCHWBL,
			AIArgs:     api.AIArgs{ChwblPrefixHashLevel: 2},
		},
	}

	if err := m.installLB(gw.client(t), lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}

	body := gw.lastBody(t)
	if !strings.Contains(body, `"sel":8`) {
		t.Errorf("gateway body missing sel=8\n%s", body)
	}
	if !strings.Contains(body, `"chwbl_prefix_hash_level":2`) {
		t.Errorf("gateway body missing chwbl level\n%s", body)
	}
}
