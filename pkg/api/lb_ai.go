package api

import (
	"fmt"
	"slices"
)

// AIArgs - loxilb-inference-gateway-only serviceArguments fields.
//
// These live FLAT inside the serviceArguments JSON object on the wire, so this
// struct MUST stay embedded in LoadBalancerService. Turning it into a named
// field (`json:"ai"`) would nest the JSON and the gateway would not see any of
// it.
//
// Every field is a value type with omitempty, so an unset AIArgs contributes
// nothing to the payload and upstream loxilb keeps seeing byte-identical
// requests. Tri-state pointers are deliberately avoided: the gateway itself
// uses the "0 means default" idiom (dpebpf_linux.go), so it does not
// distinguish an absent field from a zero one either.
//
// Field casing is copied verbatim from the gateway's api/swagger.yml and is
// deliberately inconsistent (snake_case vs camelCase). A mis-cased key is
// dropped silently by the server, with no error, so do not "tidy" these tags.
type AIArgs struct {
	// --- KV-cache exact routing (camelCase on the wire) ---

	// KvExactMode - 0=off, 1=P/D topology, 3=single role-less pool.
	KvExactMode int64 `json:"kvExactMode,omitempty"`
	// KvZmqPort - engine event socket. 1..65535, server default 5557.
	KvZmqPort int64 `json:"kvZmqPort,omitempty"`
	// KvBlockSize - KV block size in tokens. min 1, server default 16.
	KvBlockSize int64 `json:"kvBlockSize,omitempty"`
	// KvHashAlgo - "sha256_cbor" | "xxhash_cbor" | "sha256_sglang".
	// Omitting it is the recommended shape: the server then derives the value
	// from KvEngineType and the pair can never be incoherent.
	KvHashAlgo string `json:"kvHashAlgo,omitempty"`
	// KvEngineType - "vllm" (default) | "sglang". Immutable once the rule exists;
	// changing it requires delete + recreate.
	KvEngineType string `json:"kvEngineType,omitempty"`
	// KvDpRankCount - SGLang --dp-size. 1..8, 0 means server default 1.
	KvDpRankCount int32 `json:"kvDpRankCount,omitempty"`
	// KvWarmupSec - swagger documents default 30, but no Go layer applies it;
	// the handler casts the value straight through to the data plane. Send an
	// explicit value if warmup matters.
	KvWarmupSec int64 `json:"kvWarmupSec,omitempty"`

	// --- CHWBL (only meaningful with sel=chwbl(8) or sel=wrr-hash(10)) ---

	// ChwblPrefixHashLevel - 1 | 2 | 3, server default 1.
	ChwblPrefixHashLevel int `json:"chwbl_prefix_hash_level,omitempty"`
	// ChwblMeanLoadFactor - 100..300, server default 125.
	ChwblMeanLoadFactor int `json:"chwbl_mean_load_factor,omitempty"`
	// ChwblReplication - 1..1024, server default 100.
	ChwblReplication int `json:"chwbl_replication,omitempty"`
	// ChwblPrefixHashFlags - 0..255, server default 0.
	ChwblPrefixHashFlags int `json:"chwbl_prefix_hash_flags,omitempty"`
	// ChwblEnableCacheSalt - server default false.
	ChwblEnableCacheSalt bool `json:"chwbl_enable_cache_salt,omitempty"`

	// --- Prefill/Decode disaggregation ---

	// PDDisaggMode - requires mode=fullproxy and at least one prefill and one
	// decode endpoint.
	PDDisaggMode bool `json:"pd_disagg_mode,omitempty"`
	// PDCacheAwareMode - requires PDDisaggMode.
	PDCacheAwareMode bool `json:"pd_cache_aware_mode,omitempty"`
	// PDCacheThreshold - 0..100, 0 means server default 20.
	PDCacheThreshold int32 `json:"pd_cache_threshold,omitempty"`
	// PDSessionTTLSec - server default 0.
	PDSessionTTLSec int32 `json:"pd_session_ttl_sec,omitempty"`
	// PDBalanceAbsThreshold - 0 means server default 3.
	PDBalanceAbsThreshold int32 `json:"pd_balance_abs_threshold,omitempty"`

	// --- L7 / streaming / AI gateway controls ---

	// SseMode - SSE awareness, and the arming switch for AI key/limit enforcement.
	SseMode bool `json:"sse_mode,omitempty"`
	// MaxStreamDurationSec - min 0, server default 0 (unbounded).
	MaxStreamDurationSec int32 `json:"max_stream_duration_sec,omitempty"`
	// BackendKeepaliveIntervalSec - min 0, server default 0.
	BackendKeepaliveIntervalSec int32 `json:"backend_keepalive_interval_sec,omitempty"`
	// SessionHeaderName - session affinity header, e.g. "mcp-session-id".
	SessionHeaderName string `json:"session_header_name,omitempty"`
	// ModelName - model-name routing key. Empty is the catch-all rule.
	ModelName string `json:"model_name,omitempty"`
	// TraceType - e.g. "mcp".
	TraceType string `json:"trace_type,omitempty"`
	// CbEnable - per-endpoint circuit breaker.
	CbEnable bool `json:"cb_enable,omitempty"`
}

// IsSet - whether any gateway-only field carries a value.
//
// This is the gate for flavor handling: an AIArgs that IsSet() must never be
// sent to plain upstream loxilb.
//
// The comparison requires every field to be comparable. That is deliberate: a
// slice or map field would break this line at compile time, which is exactly
// when it should break, because such a field would also invalidate the shallow
// copy that StripAIFields relies on.
func (a AIArgs) IsSet() bool { return a != AIArgs{} }

// KV-cache exact routing modes.
const (
	// KvExactModeOff - KV-exact routing disabled.
	KvExactModeOff int64 = 0
	// KvExactModeZmq - ZMQ engine-event routing over a P/D topology.
	KvExactModeZmq int64 = 1
	// KvExactModeSingleRole - single role-less pool.
	KvExactModeSingleRole int64 = 3
)

// Endpoint roles for P/D disaggregation.
const (
	// EpRoleNormal - no role (default).
	EpRoleNormal int32 = 0
	// EpRolePrefill - prefill pool member.
	EpRolePrefill int32 = 1
	// EpRoleDecode - decode pool member.
	EpRoleDecode int32 = 2
)

// Validate - the full client-side check: service arguments first, then the
// endpoint roles. Use this once the payload is assembled.
func (a AIArgs) Validate(mode LbMode, eps []LoadBalancerEndpoint) error {
	if err := a.ValidateServiceArgs(mode); err != nil {
		return err
	}

	return a.ValidateEndpoints(eps)
}

// ValidateServiceArgs - the subset of the checks that does not need endpoints,
// for callers that validate while parsing configuration, before endpoint
// discovery has run.
//
// Rejects, client side, the combinations the gateway rejects server side, so
// the operator surfaces an actionable message instead of a 500 from loxilb. The
// checks and their wording mirror loxilb-inference-gateway
// pkg/loxinet/rules.go (addLbRule validation block) and the bounds declared in
// its api/swagger.yml.
//
// Zero values are never range-checked: omitempty keeps them off the wire
// entirely, which is precisely how the caller asks for the server default.
//
// Endpoint-selection algorithms (sel=8/9/10) carry their own fullproxy
// requirement, but they are validated where the sel value is resolved, not
// here.
func (a AIArgs) ValidateServiceArgs(mode LbMode) error {
	if !a.IsSet() {
		return nil
	}

	// --- P/D disaggregation ---
	if a.PDDisaggMode && mode != LBModeFullProxy {
		return fmt.Errorf("pd-disagg requires mode=fullproxy")
	}

	if a.PDCacheAwareMode && !a.PDDisaggMode {
		return fmt.Errorf("pd-cache-aware requires pd_disagg_mode=true")
	}

	// --- KV-cache exact routing ---
	switch a.KvExactMode {
	case KvExactModeOff, KvExactModeZmq, KvExactModeSingleRole:
	default:
		return fmt.Errorf("kvExactMode must be 0 (off), 1 (zmq/pd) or 3 (single-role), got %d", a.KvExactMode)
	}

	if a.KvExactMode == KvExactModeSingleRole {
		if a.PDDisaggMode {
			return fmt.Errorf("kv-exact single-role mode is incompatible with pd-disagg (use kvExactMode=1 for P/D)")
		}
		if mode != LBModeFullProxy {
			return fmt.Errorf("kv-exact single-role mode requires mode=fullproxy")
		}
	}

	if a.KvExactMode == KvExactModeZmq && !a.PDDisaggMode {
		return fmt.Errorf("kv-exact zmq mode requires pd_disagg_mode=true (use kvExactMode=3 for a single pool)")
	}

	if err := kvEngineConfigValidate(a.KvEngineType, a.KvDpRankCount); err != nil {
		return err
	}
	if err := kvHashAlgoValidate(a.KvHashAlgo, a.KvEngineType); err != nil {
		return err
	}

	// --- declared bounds (swagger), skipping zero == "use the server default" ---
	if err := rangeCheck("kvZmqPort", int64(a.KvZmqPort), 1, 65535); err != nil {
		return err
	}
	if a.KvBlockSize < 0 {
		return fmt.Errorf("kvBlockSize must be >= 1, got %d", a.KvBlockSize)
	}
	if a.KvWarmupSec < 0 {
		return fmt.Errorf("kvWarmupSec must be >= 0, got %d", a.KvWarmupSec)
	}
	if err := rangeCheck("chwbl_prefix_hash_level", int64(a.ChwblPrefixHashLevel), 1, 3); err != nil {
		return err
	}
	if err := rangeCheck("chwbl_mean_load_factor", int64(a.ChwblMeanLoadFactor), 100, 300); err != nil {
		return err
	}
	if err := rangeCheck("chwbl_replication", int64(a.ChwblReplication), 1, 1024); err != nil {
		return err
	}
	if err := rangeCheck("chwbl_prefix_hash_flags", int64(a.ChwblPrefixHashFlags), 0, 255); err != nil {
		return err
	}
	if err := rangeCheck("pd_cache_threshold", int64(a.PDCacheThreshold), 0, 100); err != nil {
		return err
	}
	if a.PDSessionTTLSec < 0 {
		return fmt.Errorf("pd_session_ttl_sec must be >= 0, got %d", a.PDSessionTTLSec)
	}
	if a.PDBalanceAbsThreshold < 0 {
		return fmt.Errorf("pd_balance_abs_threshold must be >= 0, got %d", a.PDBalanceAbsThreshold)
	}
	if a.MaxStreamDurationSec < 0 {
		return fmt.Errorf("max_stream_duration_sec must be >= 0, got %d", a.MaxStreamDurationSec)
	}
	if a.BackendKeepaliveIntervalSec < 0 {
		return fmt.Errorf("backend_keepalive_interval_sec must be >= 0, got %d", a.BackendKeepaliveIntervalSec)
	}

	return nil
}

// ValidateEndpoints - the checks that need the assembled endpoint list.
func (a AIArgs) ValidateEndpoints(eps []LoadBalancerEndpoint) error {
	if !a.IsSet() {
		return nil
	}

	for i, ep := range eps {
		switch ep.EpRole {
		case EpRoleNormal, EpRolePrefill, EpRoleDecode:
		default:
			return fmt.Errorf("endpoint[%d] %s: ep_role must be 0 (normal), 1 (prefill) or 2 (decode), got %d",
				i, ep.EndpointIP, ep.EpRole)
		}
		if ep.NixlPort < 0 || ep.NixlPort > 65535 {
			return fmt.Errorf("endpoint[%d] %s: nixl_port must be within 0..65535, got %d",
				i, ep.EndpointIP, ep.NixlPort)
		}
	}

	// loxilb refuses a disaggregated rule that cannot actually split work.
	if a.PDDisaggMode {
		hasPrefill := slices.ContainsFunc(eps, func(ep LoadBalancerEndpoint) bool {
			return ep.EpRole == EpRolePrefill
		})
		hasDecode := slices.ContainsFunc(eps, func(ep LoadBalancerEndpoint) bool {
			return ep.EpRole == EpRoleDecode
		})
		if !hasPrefill || !hasDecode {
			return fmt.Errorf("pd-disagg requires at least 1 prefill (ep_role=1) and 1 decode (ep_role=2) endpoint")
		}
	}

	return nil
}

// rangeCheck - bounds check that treats 0 as "absent"; omitempty drops it from
// the payload, so the server never sees it and applies its own default.
func rangeCheck(name string, v, min, max int64) error {
	if v == 0 || (v >= min && v <= max) {
		return nil
	}
	return fmt.Errorf("%s must be within %d..%d, got %d", name, min, max, v)
}

// kvEngineEffective - "" is an alias for the default engine, "vllm".
func kvEngineEffective(engine string) string {
	if engine == "" {
		return "vllm"
	}
	return engine
}

// kvHashAlgoEffective - mirrors the gateway's resolution order: an explicit
// algorithm always wins, otherwise the engine's default applies.
func kvHashAlgoEffective(algo, engine string) string {
	if algo != "" {
		return algo
	}
	if kvEngineEffective(engine) == "sglang" {
		return "sha256_sglang"
	}
	return "sha256_cbor"
}

func kvEngineConfigValidate(engine string, dpRankCount int32) error {
	switch engine {
	case "", "vllm", "sglang":
	default:
		return fmt.Errorf("kv-engine-type must be one of \"vllm\", \"sglang\"")
	}
	if dpRankCount < 0 || dpRankCount > 8 {
		return fmt.Errorf("kv-dp-rank-count must be within 1..8 (0 = default 1)")
	}
	return nil
}

func kvHashAlgoValidate(algo, engine string) error {
	switch algo {
	case "":
		// engine default - coherent by construction
		return nil
	case "sha256_cbor", "xxhash_cbor", "sha256_sglang":
	default:
		return fmt.Errorf("kv-hash-algo must be one of \"sha256_cbor\", \"xxhash_cbor\", \"sha256_sglang\"")
	}
	if (kvEngineEffective(engine) == "sglang") != (algo == "sha256_sglang") {
		return fmt.Errorf("kv-hash-algo %q is incompatible with kv-engine-type %q (omit kvHashAlgo to take the engine default %q)",
			algo, kvEngineEffective(engine), kvHashAlgoEffective("", engine))
	}
	return nil
}

// StripAIFields - remove every loxilb-inference-gateway-only field from a
// payload bound for plain upstream loxilb.
//
// The same LoadBalancerModel is fanned out to every client in the pool, and
// installLB copies it shallowly, so the endpoint slice's backing array is
// shared across clients. Mutating an element through that shared array would
// corrupt the payloads of the other clients. Reassigning is safe, mutating
// through is not - so this reassigns both the embedded AIArgs (a value field,
// already independent after the struct copy) and the endpoint slice (cloned
// first).
func StripAIFields(m *LoadBalancerModel) {
	m.Service.AIArgs = AIArgs{}
	m.Endpoints = stripEpRoles(m.Endpoints)
}

// stripEpRoles - copy eps with the gateway-only endpoint fields cleared.
// A shallow clone suffices because every LoadBalancerEndpoint field is a value
// type.
func stripEpRoles(src []LoadBalancerEndpoint) []LoadBalancerEndpoint {
	if src == nil {
		return nil
	}

	out := slices.Clone(src)
	for i := range out {
		out[i].EpRole = 0
		out[i].NixlPort = 0
	}

	return out
}
