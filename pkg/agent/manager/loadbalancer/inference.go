/*
 * Copyright (c) 2022 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loadbalancer

import (
	"fmt"
	"strconv"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// loxilb-inference-gateway annotations.
//
// These cover the single-pool inference use cases: prefix-cache aware routing
// (CHWBL), SGLang/vLLM KV-exact routing over one role-less pool, MCP session
// affinity and model-name routing.
//
// Prefill/decode disaggregation is deliberately absent. It needs a per-endpoint
// role (ep_role), which a Service annotation cannot express because a single
// EndpointSlice carries no way to say "this pod is prefill, that one is
// decode". Enabling pd_disagg_mode without roles is rejected by loxilb every
// time, so exposing it here could only ever produce failures. It is being
// designed separately.
const (
	// --- L7 / streaming / AI gateway ---
	modelNameAnnotation                = "loxilb.io/model-name"
	sseModeAnnotation                  = "loxilb.io/sse-mode"
	sessionHeaderNameAnnotation        = "loxilb.io/session-header-name"
	maxStreamDurationAnnotation        = "loxilb.io/max-stream-duration"
	backendKeepaliveIntervalAnnotation = "loxilb.io/backend-keepalive-interval"
	traceTypeAnnotation                = "loxilb.io/trace-type"
	cbEnableAnnotation                 = "loxilb.io/cb-enable"

	// --- CHWBL, for sel=chwbl and sel=wrr-hash ---
	chwblPrefixHashLevelAnnotation = "loxilb.io/chwbl-prefix-hash-level"
	chwblPrefixHashFlagsAnnotation = "loxilb.io/chwbl-prefix-hash-flags"
	chwblMeanLoadFactorAnnotation  = "loxilb.io/chwbl-mean-load-factor"
	chwblReplicationAnnotation     = "loxilb.io/chwbl-replication"
	chwblEnableCacheSaltAnnotation = "loxilb.io/chwbl-enable-cache-salt"

	// --- KV-cache exact routing ---
	kvExactModeAnnotation   = "loxilb.io/kv-exact-mode"
	kvZmqPortAnnotation     = "loxilb.io/kv-zmq-port"
	kvBlockSizeAnnotation   = "loxilb.io/kv-block-size"
	kvHashAlgoAnnotation    = "loxilb.io/kv-hash-algo"
	kvEngineTypeAnnotation  = "loxilb.io/kv-engine-type"
	kvDpRankCountAnnotation = "loxilb.io/kv-dp-rank-count"
	kvWarmupSecAnnotation   = "loxilb.io/kv-warmup-sec"
)

// aiAnnotations - every annotation handled by getAIArgs, used to tell "the user
// asked for inference routing" from "the user asked for nothing".
var aiAnnotations = []string{
	modelNameAnnotation, sseModeAnnotation, sessionHeaderNameAnnotation,
	maxStreamDurationAnnotation, backendKeepaliveIntervalAnnotation,
	traceTypeAnnotation, cbEnableAnnotation,
	chwblPrefixHashLevelAnnotation, chwblPrefixHashFlagsAnnotation,
	chwblMeanLoadFactorAnnotation, chwblReplicationAnnotation,
	chwblEnableCacheSaltAnnotation,
	kvExactModeAnnotation, kvZmqPortAnnotation, kvBlockSizeAnnotation,
	kvHashAlgoAnnotation, kvEngineTypeAnnotation, kvDpRankCountAnnotation,
	kvWarmupSecAnnotation,
}

// hasAIAnnotation - whether the service asks for anything gateway-specific,
// counting the gateway-only endpoint selectors as well.
func hasAIAnnotation(svc *corev1.Service) bool {
	for _, a := range aiAnnotations {
		if svc.Annotations[a] != "" {
			return true
		}
	}

	switch svc.Annotations[endPointSelAnnotation] {
	case "chwbl", "gpuaware", "wrr-hash", "wrrhash":
		return true
	}

	return false
}

// annoInt - parse an integer annotation. An absent annotation yields 0, which
// every AIArgs field treats as "unset", so the server applies its own default.
func annoInt(svc *corev1.Service, key string) (int64, error) {
	raw, ok := svc.Annotations[key]
	if !ok || raw == "" {
		return 0, nil
	}

	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, raw)
	}

	return v, nil
}

// annoBool - parse a boolean annotation, accepting the yes/no spelling the
// older loxilb annotations use alongside true/false.
func annoBool(svc *corev1.Service, key string) (bool, error) {
	raw, ok := svc.Annotations[key]
	if !ok || raw == "" {
		return false, nil
	}

	switch raw {
	case "true", "yes", "1":
		return true, nil
	case "false", "no", "0":
		return false, nil
	}

	return false, fmt.Errorf("%s: %q is not a boolean (use true/false or yes/no)", key, raw)
}

// effectiveLbMode - the mode the rule will actually carry: the per-service
// annotation when set, otherwise the agent-wide default. Mirrors
// makeLoxiLoadBalancerModel.
func (m *Manager) effectiveLbMode(lbMode int) api.LbMode {
	if lbMode >= 0 {
		return api.LbMode(lbMode)
	}

	return api.LbMode(m.networkConfig.SetLBMode)
}

// getAIArgs - build the gateway-only service arguments from annotations.
//
// Returns a zero AIArgs when the service asks for nothing, which keeps the
// payload byte-identical to a plain loxilb rule.
func (m *Manager) getAIArgs(svc *corev1.Service, lbMode int, sel api.EpSelect) (api.AIArgs, error) {
	var aiArgs api.AIArgs

	if !hasAIAnnotation(svc) {
		return aiArgs, nil
	}

	var errs []error
	intVal := func(key string) int64 {
		v, err := annoInt(svc, key)
		if err != nil {
			errs = append(errs, err)
		}
		return v
	}
	boolVal := func(key string) bool {
		v, err := annoBool(svc, key)
		if err != nil {
			errs = append(errs, err)
		}
		return v
	}

	aiArgs.ModelName = svc.Annotations[modelNameAnnotation]
	aiArgs.SessionHeaderName = svc.Annotations[sessionHeaderNameAnnotation]
	aiArgs.TraceType = svc.Annotations[traceTypeAnnotation]
	aiArgs.KvHashAlgo = svc.Annotations[kvHashAlgoAnnotation]
	aiArgs.KvEngineType = svc.Annotations[kvEngineTypeAnnotation]

	aiArgs.SseMode = boolVal(sseModeAnnotation)
	aiArgs.CbEnable = boolVal(cbEnableAnnotation)
	aiArgs.ChwblEnableCacheSalt = boolVal(chwblEnableCacheSaltAnnotation)

	aiArgs.MaxStreamDurationSec = int32(intVal(maxStreamDurationAnnotation))
	aiArgs.BackendKeepaliveIntervalSec = int32(intVal(backendKeepaliveIntervalAnnotation))

	aiArgs.ChwblPrefixHashLevel = int(intVal(chwblPrefixHashLevelAnnotation))
	aiArgs.ChwblPrefixHashFlags = int(intVal(chwblPrefixHashFlagsAnnotation))
	aiArgs.ChwblMeanLoadFactor = int(intVal(chwblMeanLoadFactorAnnotation))
	aiArgs.ChwblReplication = int(intVal(chwblReplicationAnnotation))

	aiArgs.KvExactMode = intVal(kvExactModeAnnotation)
	aiArgs.KvZmqPort = intVal(kvZmqPortAnnotation)
	aiArgs.KvBlockSize = intVal(kvBlockSizeAnnotation)
	aiArgs.KvWarmupSec = intVal(kvWarmupSecAnnotation)
	aiArgs.KvDpRankCount = int32(intVal(kvDpRankCountAnnotation))

	if len(errs) > 0 {
		return api.AIArgs{}, errs[0]
	}

	mode := m.effectiveLbMode(lbMode)

	// The gateway-only selectors have no L4 datapath implementation: outside
	// fullproxy the rule is programmed as DNAT, the kernel selector matches no
	// case and every SYN is black-holed. loxilb rejects this; say so first.
	if sel.IsInferenceGatewayOnly() && mode != api.LBModeFullProxy {
		return api.AIArgs{}, fmt.Errorf("%s=%s requires %s=fullproxy",
			endPointSelAnnotation, svc.Annotations[endPointSelAnnotation], lbModeAnnotation)
	}

	// Endpoints are not known yet; the endpoint-role rules are re-checked
	// against the real payload in makeLoxiLoadBalancerModel.
	if err := aiArgs.Validate(mode, nil); err != nil {
		return api.AIArgs{}, err
	}

	return aiArgs, nil
}

// ErrInferenceGatewayRequired - the peer cannot serve the requested routing.
// Sentinel so the fan-out can tell this refusal apart from a transport error
// and report it on the Service.
var ErrInferenceGatewayRequired = errors.New("loxilb peer is not a loxilb-inference-gateway")

// Event reasons raised against a Service.
const (
	// ReasonInvalidInferenceConfig - the inference annotations are malformed or
	// describe a combination loxilb rejects.
	ReasonInvalidInferenceConfig = "InvalidInferenceConfig"
	// ReasonInferenceGatewayRequired - the service asks for inference routing
	// but the loxilb it would be programmed into is plain upstream loxilb.
	ReasonInferenceGatewayRequired = "InferenceGatewayRequired"
)

// recordServiceWarning - surface a rejection on the Service itself, so
// `kubectl describe svc` explains it. A log line on the agent does not reach
// the person who wrote the annotation.
func (m *Manager) recordServiceWarning(svc *corev1.Service, reason, message string) {
	if m.eventRecorder == nil || svc == nil {
		return
	}

	m.eventRecorder.Event(svc, corev1.EventTypeWarning, reason, message)
}
