package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// aiWireKeys - every gateway-only key that must never reach plain loxilb.
var aiWireKeys = []string{
	"kvExactMode", "kvZmqPort", "kvBlockSize", "kvHashAlgo", "kvEngineType",
	"kvDpRankCount", "kvWarmupSec",
	"chwbl_prefix_hash_level", "chwbl_mean_load_factor", "chwbl_replication",
	"chwbl_prefix_hash_flags", "chwbl_enable_cache_salt",
	"pd_disagg_mode", "pd_cache_aware_mode", "pd_cache_threshold",
	"pd_session_ttl_sec", "pd_balance_abs_threshold",
	"sse_mode", "max_stream_duration_sec", "backend_keepalive_interval_sec",
	"session_header_name", "model_name", "trace_type", "cb_enable",
	"ep_role", "nixl_port",
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func assertNoAIKeys(t *testing.T, label, payload string) {
	t.Helper()

	for _, k := range aiWireKeys {
		if strings.Contains(payload, k) {
			t.Errorf("%s: payload carries gateway-only key %q\n%s", label, k, payload)
		}
	}
}

// Acceptance criterion 6: an unset AIArgs must not appear in the payload at
// all, so upstream loxilb keeps receiving exactly what it received before.
func TestUnsetAIArgsProducesNoWireFields(t *testing.T) {
	lb := LoadBalancerModel{
		Service: LoadBalancerService{
			ExternalIP: "10.0.0.2",
			Port:       80,
			Protocol:   "tcp",
		},
		Endpoints: []LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1},
		},
	}

	assertNoAIKeys(t, "unset AIArgs", mustMarshal(t, &lb))

	if lb.Service.AIArgs.IsSet() {
		t.Error("IsSet() = true for a zero AIArgs")
	}
}

// The wire contract is flat: AIArgs must serialize into serviceArguments
// itself, not into a nested object.
func TestAIArgsSerializeFlat(t *testing.T) {
	svc := LoadBalancerService{
		ExternalIP: "10.0.0.1",
		Port:       8080,
		AIArgs: AIArgs{
			KvExactMode:  KvExactModeZmq,
			PDDisaggMode: true,
			SseMode:      true,
		},
	}

	payload := mustMarshal(t, &svc)

	for _, want := range []string{`"kvExactMode":1`, `"pd_disagg_mode":true`, `"sse_mode":true`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s\n%s", want, payload)
		}
	}
	if strings.Contains(payload, `"AIArgs"`) || strings.Contains(payload, `"ai"`) {
		t.Errorf("AIArgs must not nest on the wire\n%s", payload)
	}
	if !svc.AIArgs.IsSet() {
		t.Error("IsSet() = false for a populated AIArgs")
	}
}

// Promoted access must work, so manager code reads svc.KvExactMode directly.
func TestAIArgsPromotedAccess(t *testing.T) {
	svc := LoadBalancerService{}
	svc.KvExactMode = KvExactModeSingleRole

	if svc.AIArgs.KvExactMode != KvExactModeSingleRole {
		t.Errorf("promoted write did not reach AIArgs")
	}
}

// The embedded struct must stay invisible to the reflection that builds delete
// sub-resources and query params.
func TestAIArgsDoesNotDisturbKeyReflection(t *testing.T) {
	lb := &LoadBalancerModel{
		Service: LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			BGP:        true,
			Block:      7,
			AIArgs:     AIArgs{SseMode: true},
		},
	}

	c := &APICommonFunc{}
	sub, err := c.MakeDeletedSubResource([]string{"externalipaddress", "port", "protocol"}, lb)
	if err != nil {
		t.Fatalf("MakeDeletedSubResource: %v", err)
	}
	if want := "externalipaddress/10.0.0.1/port/8080/protocol/tcp"; sub != want {
		t.Errorf("sub-resource = %q, want %q", sub, want)
	}

	q, err := c.MakeQueryParam(lb)
	if err != nil {
		t.Fatalf("MakeQueryParam: %v", err)
	}
	if q["bgp"] != "true" || q["block"] != "7" {
		t.Errorf("query params = %v, want bgp=true block=7", q)
	}
}

func TestStripGatewayFieldsLeavesSourceIntact(t *testing.T) {
	src := LoadBalancerModel{
		Service: LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			AIArgs:     AIArgs{KvExactMode: KvExactModeZmq, PDDisaggMode: true},
		},
		Endpoints: []LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, EpRole: EpRolePrefill, NixlPort: 9001},
			{EndpointIP: "31.31.31.2", TargetPort: 8000, EpRole: EpRoleDecode},
		},
	}
	before := mustMarshal(t, &src)

	// exactly how installLB copies the model
	stripped := src
	StripGatewayFields(&stripped)

	assertNoAIKeys(t, "stripped", mustMarshal(t, &stripped))

	if after := mustMarshal(t, &src); after != before {
		t.Errorf("StripGatewayFields mutated the source model\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestStripGatewayFieldsNilEndpoints(t *testing.T) {
	m := LoadBalancerModel{Service: LoadBalancerService{AIArgs: AIArgs{SseMode: true}}}
	StripGatewayFields(&m)

	if m.Endpoints != nil {
		t.Errorf("nil endpoints became %v", m.Endpoints)
	}
	if m.Service.AIArgs.IsSet() {
		t.Error("AIArgs survived stripping")
	}
}

func pdEndpoints() []LoadBalancerEndpoint {
	return []LoadBalancerEndpoint{
		{EndpointIP: "31.31.31.1", EpRole: EpRolePrefill},
		{EndpointIP: "31.31.31.2", EpRole: EpRoleDecode},
	}
}

func TestAIArgsValidate(t *testing.T) {
	tests := []struct {
		name    string
		args    AIArgs
		mode    LbMode
		eps     []LoadBalancerEndpoint
		wantErr string
	}{
		{
			name: "unset args always pass",
			mode: LBModeDefault,
		},
		{
			name: "pd-disagg happy path",
			args: AIArgs{PDDisaggMode: true},
			mode: LBModeFullProxy,
			eps:  pdEndpoints(),
		},
		{
			name:    "pd-disagg outside fullproxy",
			args:    AIArgs{PDDisaggMode: true},
			mode:    LBModeFullNat,
			eps:     pdEndpoints(),
			wantErr: "pd-disagg requires mode=fullproxy",
		},
		{
			name:    "pd-disagg without a decode endpoint",
			args:    AIArgs{PDDisaggMode: true},
			mode:    LBModeFullProxy,
			eps:     []LoadBalancerEndpoint{{EndpointIP: "31.31.31.1", EpRole: EpRolePrefill}},
			wantErr: "at least 1 prefill (ep_role=1) and 1 decode (ep_role=2)",
		},
		{
			name:    "cache-aware without disagg",
			args:    AIArgs{PDCacheAwareMode: true},
			mode:    LBModeFullProxy,
			wantErr: "pd-cache-aware requires pd_disagg_mode=true",
		},
		{
			name:    "single-role kv-exact with pd-disagg",
			args:    AIArgs{KvExactMode: KvExactModeSingleRole, PDDisaggMode: true},
			mode:    LBModeFullProxy,
			eps:     pdEndpoints(),
			wantErr: "incompatible with pd-disagg",
		},
		{
			name:    "single-role kv-exact outside fullproxy",
			args:    AIArgs{KvExactMode: KvExactModeSingleRole},
			mode:    LBModeDefault,
			wantErr: "kv-exact single-role mode requires mode=fullproxy",
		},
		{
			name:    "zmq kv-exact without pd-disagg",
			args:    AIArgs{KvExactMode: KvExactModeZmq},
			mode:    LBModeFullProxy,
			wantErr: "use kvExactMode=3 for a single pool",
		},
		{
			name:    "kv-exact out of range",
			args:    AIArgs{KvExactMode: 2, PDDisaggMode: true},
			mode:    LBModeFullProxy,
			eps:     pdEndpoints(),
			wantErr: "kvExactMode must be 0 (off), 1 (zmq/pd) or 3 (single-role)",
		},
		{
			name:    "unknown engine",
			args:    AIArgs{KvEngineType: "trtllm", SseMode: true},
			mode:    LBModeFullProxy,
			wantErr: `kv-engine-type must be one of "vllm", "sglang"`,
		},
		{
			name:    "dp rank above 8",
			args:    AIArgs{KvDpRankCount: 9, SseMode: true},
			mode:    LBModeFullProxy,
			wantErr: "kv-dp-rank-count must be within 1..8",
		},
		{
			name: "dp rank 0 means server default",
			args: AIArgs{KvDpRankCount: 0, SseMode: true},
			mode: LBModeFullProxy,
		},
		{
			name:    "hash algo incoherent with engine",
			args:    AIArgs{KvEngineType: "sglang", KvHashAlgo: "sha256_cbor"},
			mode:    LBModeFullProxy,
			wantErr: "is incompatible with kv-engine-type",
		},
		{
			name: "omitted hash algo is always coherent",
			args: AIArgs{KvEngineType: "sglang"},
			mode: LBModeFullProxy,
		},
		{
			name:    "chwbl mean load factor below range",
			args:    AIArgs{ChwblMeanLoadFactor: 50},
			mode:    LBModeFullProxy,
			wantErr: "chwbl_mean_load_factor must be within 100..300",
		},
		{
			name: "chwbl mean load factor zero takes the server default",
			args: AIArgs{ChwblReplication: 100},
			mode: LBModeFullProxy,
		},
		{
			name:    "cache threshold above 100",
			args:    AIArgs{PDCacheThreshold: 101, SseMode: true},
			mode:    LBModeFullProxy,
			wantErr: "pd_cache_threshold must be within 0..100",
		},
		{
			name:    "bad endpoint role",
			args:    AIArgs{SseMode: true},
			mode:    LBModeFullProxy,
			eps:     []LoadBalancerEndpoint{{EndpointIP: "31.31.31.1", EpRole: 5}},
			wantErr: "ep_role must be 0 (normal), 1 (prefill) or 2 (decode)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.args.Validate(tt.mode, tt.eps)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
