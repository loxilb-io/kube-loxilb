package api

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// FlowControlArgs - the capacity admission gate of loxilb-inference-gateway
// (the fc_* serviceArguments): how many inference requests may execute on the
// rule's model pool, how many may wait for a turn, and in what order.
//
// Embedded in AIArgs, so the fields sit flat in serviceArguments, AIArgs.IsSet
// counts them and StripGatewayFields clears them. Same contract as AIArgs:
// value types with omitempty, comparable, and 0 or "" leaves the field out so
// the gateway runs on its process default (LLB_FC_*). kube-loxilb only ever
// creates a rule (an annotation change is a delete and an add), so the
// gateway's replace semantics for omitted fields never come into play.
//
// The gateway consults the gate on AI-gateway services only - see
// AIArgs.IsAIGatewayService.
type FlowControlArgs struct {
	// FcMode - "off" | "observe" | "enforce" | "inherit".
	FcMode string `json:"fc_mode,omitempty"`

	// --- ceilings, each 0..100000 ---

	// FcMaxOutstanding - executing requests on the whole pool.
	FcMaxOutstanding int64 `json:"fc_max_outstanding,omitempty"`
	// FcEpMaxInflight - executing requests per endpoint, normal role.
	FcEpMaxInflight int64 `json:"fc_ep_max_inflight,omitempty"`
	// FcPrefillMaxInflight - prefill legs per endpoint.
	FcPrefillMaxInflight int64 `json:"fc_prefill_max_inflight,omitempty"`
	// FcDecodeMaxInflight - decode legs per endpoint.
	FcDecodeMaxInflight int64 `json:"fc_decode_max_inflight,omitempty"`

	// --- queue ---

	// FcMaxQueueDepth - requests that may wait for a unit, 0..65536. Each one
	// parks a client connection of about 1 MiB.
	FcMaxQueueDepth int64 `json:"fc_max_queue_depth,omitempty"`
	// FcMaxQueueWaitMs - longest wait before 504, 0..3600000. Required with a
	// depth.
	FcMaxQueueWaitMs int64 `json:"fc_max_queue_wait_ms,omitempty"`

	// --- adaptive ceiling and endpoint ramp ---

	// FcAdaptive - "on" | "off" | "inherit".
	FcAdaptive string `json:"fc_adaptive,omitempty"`
	// FcTtftTargetMs - time to first token that counts as backpressure.
	FcTtftTargetMs int64 `json:"fc_ttft_target_ms,omitempty"`
	// FcTelemetryStaleMs - how long scraped endpoint telemetry is trusted.
	FcTelemetryStaleMs int64 `json:"fc_telemetry_stale_ms,omitempty"`
	// FcWarmupMs - ramp of a returning endpoint's ceilings.
	FcWarmupMs int64 `json:"fc_warmup_ms,omitempty"`

	// --- tenants and response headers ---

	// FcTenantMaxSharePct - most of the ceiling and queue one tenant holds.
	FcTenantMaxSharePct int64 `json:"fc_tenant_max_share_pct,omitempty"`
	// FcTenantWake - "fifo" | "rr" | "inherit".
	FcTenantWake string `json:"fc_tenant_wake,omitempty"`
	// FcExposeHeaders - "on" | "off" | "inherit": X-Loxilb-Admission-* on
	// admitted responses.
	FcExposeHeaders string `json:"fc_expose_headers,omitempty"`
	// FcLlmdHeaders - "on" | "off" | "inherit": the llm-d objective,
	// fairness-id and TTL headers.
	FcLlmdHeaders string `json:"fc_llmd_headers,omitempty"`

	// --- priority bands (critical, standard, sheddable) ---
	//
	// The triples are written "critical:standard:sheddable".

	// FcWakePolicy - "strict" | "weighted" | "inherit".
	FcWakePolicy string `json:"fc_wake_policy,omitempty"`
	// FcBandWeights - weighted wake credits per round, e.g. "8:3:1".
	FcBandWeights string `json:"fc_band_weights,omitempty"`
	// FcBandMinSharePct - each band's floor under contention.
	FcBandMinSharePct string `json:"fc_band_min_share_pct,omitempty"`
	// FcCriticalReservePct - units no other band may take, 0..100.
	FcCriticalReservePct int64 `json:"fc_critical_reserve_pct,omitempty"`
	// FcBandWaitMs - each band's wait window inside FcMaxQueueWaitMs.
	FcBandWaitMs string `json:"fc_band_wait_ms,omitempty"`
	// FcBandQueuePct - each band's share of the queue depth.
	FcBandQueuePct string `json:"fc_band_queue_pct,omitempty"`
	// FcShedQueue - "on" | "off" | "inherit": sheddable requests wait, and
	// are evicted by higher bands when the queue is full.
	FcShedQueue string `json:"fc_shed_queue,omitempty"`
}

// IsSet - whether any of the gate's fields carries a value.
func (f FlowControlArgs) IsSet() bool { return f != FlowControlArgs{} }

// Bounds of the gate's fields, mirroring loxilb-inference-gateway
// common/fc_queue.go and api/swagger.yml.
const (
	// FcCapMax - upper bound of every ceiling.
	FcCapMax = 100000
	// FcQueueDepthMax - upper bound of FcMaxQueueDepth.
	FcQueueDepthMax = 65536
	// FcMsMax - upper bound of every window in milliseconds (an hour).
	FcMsMax = 3600000
	// FcPctMax - upper bound of every percentage.
	FcPctMax = 100
	// FcBandWeightMax - upper bound of one band weight.
	FcBandWeightMax = 62
)

// validate - the gateway's per-field checks on the gate, client side and in
// its wording: the enums, the declared bounds, the shape of the band triples,
// and the one rule between the queue's two fields.
//
// The rules that span the band triples (weights, floors and the reserve
// summed, weighted without a weight per band) are left to the gateway, and so
// is fc_shed_queue on without a queue, which depends on its environment. Its
// refusal reaches the Service as LoxiLBRejected.
func (f FlowControlArgs) validate() error {
	for _, e := range []struct {
		field, value string
		allowed      []string
	}{
		{"fc_mode", f.FcMode, []string{"off", "observe", "enforce", "inherit"}},
		{"fc_adaptive", f.FcAdaptive, []string{"on", "off", "inherit"}},
		{"fc_expose_headers", f.FcExposeHeaders, []string{"on", "off", "inherit"}},
		{"fc_llmd_headers", f.FcLlmdHeaders, []string{"on", "off", "inherit"}},
		{"fc_shed_queue", f.FcShedQueue, []string{"on", "off", "inherit"}},
		{"fc_tenant_wake", f.FcTenantWake, []string{"fifo", "rr", "inherit"}},
		{"fc_wake_policy", f.FcWakePolicy, []string{"strict", "weighted", "inherit"}},
	} {
		if err := fcEnumCheck(e.field, e.value, e.allowed); err != nil {
			return err
		}
	}

	for _, r := range []struct {
		field string
		v     int64
		max   int64
	}{
		{"fc_max_outstanding", f.FcMaxOutstanding, FcCapMax},
		{"fc_ep_max_inflight", f.FcEpMaxInflight, FcCapMax},
		{"fc_prefill_max_inflight", f.FcPrefillMaxInflight, FcCapMax},
		{"fc_decode_max_inflight", f.FcDecodeMaxInflight, FcCapMax},
		{"fc_max_queue_depth", f.FcMaxQueueDepth, FcQueueDepthMax},
		{"fc_max_queue_wait_ms", f.FcMaxQueueWaitMs, FcMsMax},
		{"fc_ttft_target_ms", f.FcTtftTargetMs, FcMsMax},
		{"fc_telemetry_stale_ms", f.FcTelemetryStaleMs, FcMsMax},
		{"fc_warmup_ms", f.FcWarmupMs, FcMsMax},
		{"fc_tenant_max_share_pct", f.FcTenantMaxSharePct, FcPctMax},
		{"fc_critical_reserve_pct", f.FcCriticalReservePct, FcPctMax},
	} {
		if r.v < 0 || r.v > r.max {
			return fmt.Errorf("%s must be within 0..%d, got %d", r.field, r.max, r.v)
		}
	}

	if f.FcMaxQueueDepth > 0 && f.FcMaxQueueWaitMs == 0 {
		return fmt.Errorf("fc_max_queue_wait_ms must be greater than 0 when fc_max_queue_depth is set")
	}

	for _, t := range []struct {
		field, value string
		limit        uint64
	}{
		{"fc_band_weights", f.FcBandWeights, FcBandWeightMax},
		{"fc_band_min_share_pct", f.FcBandMinSharePct, FcPctMax},
		{"fc_band_wait_ms", f.FcBandWaitMs, FcMsMax},
		{"fc_band_queue_pct", f.FcBandQueuePct, FcPctMax},
	} {
		if err := fcBandTripleCheck(t.field, t.value, t.limit); err != nil {
			return err
		}
	}

	return nil
}

// fcEnumCheck - "" is the field left out; anything else must be one of allowed.
func fcEnumCheck(field, value string, allowed []string) error {
	if value == "" || slices.Contains(allowed, value) {
		return nil
	}

	last := len(allowed) - 1
	return fmt.Errorf("%s must be one of %s or %s, got %q",
		field, strings.Join(allowed[:last], ", "), allowed[last], value)
}

// fcBandTripleCheck - a band triple is three non-negative whole numbers
// separated by ':', each at most limit. Mirrors FcBandTripleParse.
func fcBandTripleCheck(field, value string, limit uint64) error {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return fmt.Errorf("%s must be three whole numbers critical:standard:sheddable, got %q", field, value)
	}
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return fmt.Errorf("%s must be three whole numbers critical:standard:sheddable, got %q", field, value)
		}
		if n > limit {
			return fmt.Errorf("%s: each value must be within 0..%d, got %q", field, limit, value)
		}
	}

	return nil
}
