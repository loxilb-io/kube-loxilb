package api

import (
	"strings"
	"testing"
)

// The gate's fields sit flat in serviceArguments under the gateway's names,
// as in a request the gateway is known to take.
func TestFlowControlWireFields(t *testing.T) {
	svc := LoadBalancerService{
		ExternalIP: "192.168.0.13",
		Port:       8081,
		AIArgs: AIArgs{
			SseMode: true,
			FlowControlArgs: FlowControlArgs{
				FcMode:           "enforce",
				FcEpMaxInflight:  32,
				FcMaxOutstanding: 96,
				FcMaxQueueDepth:  96,
				FcMaxQueueWaitMs: 15000,
				FcWarmupMs:       30000,
				FcAdaptive:       "on",
				FcTtftTargetMs:   5000,
				FcExposeHeaders:  "on",
				FcBandWeights:    "8:3:1",
			},
		},
	}

	payload := mustMarshal(t, &svc)
	for _, want := range []string{
		`"fc_mode":"enforce"`, `"fc_ep_max_inflight":32`, `"fc_max_outstanding":96`,
		`"fc_max_queue_depth":96`, `"fc_max_queue_wait_ms":15000`, `"fc_warmup_ms":30000`,
		`"fc_adaptive":"on"`, `"fc_ttft_target_ms":5000`, `"fc_expose_headers":"on"`,
		`"fc_band_weights":"8:3:1"`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s\n%s", want, payload)
		}
	}
	if strings.Contains(payload, "FlowControlArgs") {
		t.Errorf("FlowControlArgs must not nest on the wire\n%s", payload)
	}
	for _, unset := range []string{"fc_decode_max_inflight", "fc_wake_policy", "fc_shed_queue"} {
		if strings.Contains(payload, unset) {
			t.Errorf("payload carries unset %s\n%s", unset, payload)
		}
	}
}

// The gate alone makes AIArgs set, so a rule carrying it is never sent to a
// plain loxilb.
func TestFlowControlCountsAsGatewayOnly(t *testing.T) {
	if !(AIArgs{FlowControlArgs: FlowControlArgs{FcMode: "observe"}}).IsSet() {
		t.Error("AIArgs.IsSet() = false with only fc_mode set")
	}
}

func TestIsAIGatewayService(t *testing.T) {
	tests := []struct {
		name string
		args AIArgs
		want bool
	}{
		{name: "nothing", args: AIArgs{}},
		{name: "model name and chwbl tuning only", args: AIArgs{ModelName: "m", ChwblReplication: 100}},
		{name: "endpoint picker only", args: AIArgs{EppEndpoint: "10.43.0.9:9002"}},
		{name: "api-key-auth disabled", args: AIArgs{APIKeyAuth: APIKeyAuthDisabled}},
		{name: "sse mode", args: AIArgs{SseMode: true}, want: true},
		{name: "pd disagg", args: AIArgs{PDDisaggMode: true}, want: true},
		{name: "api-key-auth required", args: AIArgs{APIKeyAuth: APIKeyAuthRequired}, want: true},
		{name: "api-key-auth jwt", args: AIArgs{APIKeyAuth: APIKeyAuthJWT, JwtAuthProfile: "kc"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.IsAIGatewayService(); got != tt.want {
				t.Errorf("IsAIGatewayService() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFlowControlValidate(t *testing.T) {
	tests := []struct {
		name    string
		fc      FlowControlArgs
		wantErr string
	}{
		{name: "unset"},
		{
			name: "the gateway's own example",
			fc: FlowControlArgs{FcMode: "enforce", FcEpMaxInflight: 32, FcMaxOutstanding: 96,
				FcMaxQueueDepth: 96, FcMaxQueueWaitMs: 15000, FcWarmupMs: 30000,
				FcAdaptive: "on", FcTtftTargetMs: 5000, FcExposeHeaders: "on"},
		},
		{
			name: "every enum at inherit",
			fc: FlowControlArgs{FcMode: "inherit", FcAdaptive: "inherit", FcExposeHeaders: "inherit",
				FcLlmdHeaders: "inherit", FcShedQueue: "inherit", FcTenantWake: "inherit", FcWakePolicy: "inherit"},
		},
		{
			name: "bands at their bounds",
			fc: FlowControlArgs{FcWakePolicy: "weighted", FcBandWeights: "62:1:1", FcBandMinSharePct: "100:0:0",
				FcBandWaitMs: "3600000:0:0", FcBandQueuePct: "0:0:0", FcCriticalReservePct: 100},
		},
		{
			name:    "unknown mode",
			fc:      FlowControlArgs{FcMode: "on"},
			wantErr: `fc_mode must be one of off, observe, enforce or inherit, got "on"`,
		},
		{
			name:    "unknown switch",
			fc:      FlowControlArgs{FcShedQueue: "true"},
			wantErr: "fc_shed_queue must be one of on, off or inherit",
		},
		{
			name:    "unknown tenant wake",
			fc:      FlowControlArgs{FcTenantWake: "lifo"},
			wantErr: "fc_tenant_wake must be one of fifo, rr or inherit",
		},
		{
			name:    "ceiling over its bound",
			fc:      FlowControlArgs{FcMaxOutstanding: FcCapMax + 1},
			wantErr: "fc_max_outstanding must be within 0..100000",
		},
		{
			name:    "negative ceiling",
			fc:      FlowControlArgs{FcEpMaxInflight: -1},
			wantErr: "fc_ep_max_inflight must be within 0..100000",
		},
		{
			name:    "queue deeper than its bound",
			fc:      FlowControlArgs{FcMaxQueueDepth: FcQueueDepthMax + 1, FcMaxQueueWaitMs: 1000},
			wantErr: "fc_max_queue_depth must be within 0..65536",
		},
		{
			name:    "share over 100",
			fc:      FlowControlArgs{FcTenantMaxSharePct: 101},
			wantErr: "fc_tenant_max_share_pct must be within 0..100",
		},
		{
			name:    "window over an hour",
			fc:      FlowControlArgs{FcWarmupMs: FcMsMax + 1},
			wantErr: "fc_warmup_ms must be within 0..3600000",
		},
		{
			name:    "a queue without a wait window",
			fc:      FlowControlArgs{FcMaxQueueDepth: 96},
			wantErr: "fc_max_queue_wait_ms must be greater than 0 when fc_max_queue_depth is set",
		},
		{
			name: "a wait window without a queue",
			fc:   FlowControlArgs{FcMaxQueueWaitMs: 15000},
		},
		{
			name:    "two numbers for three bands",
			fc:      FlowControlArgs{FcBandWeights: "8:3"},
			wantErr: `fc_band_weights must be three whole numbers critical:standard:sheddable, got "8:3"`,
		},
		{
			name:    "a band that is not a number",
			fc:      FlowControlArgs{FcBandMinSharePct: "50:x:0"},
			wantErr: "fc_band_min_share_pct must be three whole numbers",
		},
		{
			name:    "a negative band value",
			fc:      FlowControlArgs{FcBandWaitMs: "-1:0:0"},
			wantErr: "fc_band_wait_ms must be three whole numbers",
		},
		{
			name:    "a weight over its bound",
			fc:      FlowControlArgs{FcBandWeights: "63:1:0"},
			wantErr: `fc_band_weights: each value must be within 0..62, got "63:1:0"`,
		},
		{
			name:    "a queue share over 100",
			fc:      FlowControlArgs{FcBandQueuePct: "0:101:0"},
			wantErr: "fc_band_queue_pct: each value must be within 0..100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AIArgs{SseMode: true, FlowControlArgs: tt.fc}.ValidateServiceArgs(LBModeFullProxy)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateServiceArgs() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateServiceArgs() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateServiceArgs() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
