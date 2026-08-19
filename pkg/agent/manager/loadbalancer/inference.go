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
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

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

	// --- Prefill/decode disaggregation ---
	pdDisaggAnnotation              = "loxilb.io/pd-disagg"
	pdPrefillSelectorAnnotation     = "loxilb.io/pd-prefill-selector"
	pdDecodeSelectorAnnotation      = "loxilb.io/pd-decode-selector"
	pdPrefillNixlPortAnnotation     = "loxilb.io/pd-prefill-nixl-port"
	pdDecodeNixlPortAnnotation      = "loxilb.io/pd-decode-nixl-port"
	pdCacheAwareAnnotation          = "loxilb.io/pd-cache-aware"
	pdCacheThresholdAnnotation      = "loxilb.io/pd-cache-threshold"
	pdSessionTTLAnnotation          = "loxilb.io/pd-session-ttl"
	pdBalanceAbsThresholdAnnotation = "loxilb.io/pd-balance-abs-threshold"

	// --- KV-cache exact routing ---
	kvExactModeAnnotation   = "loxilb.io/kv-exact-mode"
	kvZmqPortAnnotation     = "loxilb.io/kv-zmq-port"
	kvBlockSizeAnnotation   = "loxilb.io/kv-block-size"
	kvHashAlgoAnnotation    = "loxilb.io/kv-hash-algo"
	kvEngineTypeAnnotation  = "loxilb.io/kv-engine-type"
	kvDpRankCountAnnotation = "loxilb.io/kv-dp-rank-count"
	kvWarmupSecAnnotation   = "loxilb.io/kv-warmup-sec"
)

// Endpoint health-monitor annotations.
//
// These configure the gateway's per-endpoint health monitor. They are named
// inside the existing probe family rather than after the wire fields, so the
// whole probe surface reads as one group.
//
// They do not replace loxilb.io/probereq and loxilb.io/proberesp: the gateway
// keeps those as its escape hatch, and a field set here simply wins over them.
const (
	probeMethodAnnotation        = "loxilb.io/probe-method"
	probePathAnnotation          = "loxilb.io/probe-path"
	probeExpectedCodesAnnotation = "loxilb.io/probe-expected-codes"
	probeHTTPVersionAnnotation   = "loxilb.io/probe-http-version"
	probeDomainAnnotation        = "loxilb.io/probe-domain"
)

// getEndpointProbe - build the health-monitor block from annotations.
//
// The values are uniform across the pool: one health-check path and one set of
// expected codes for every endpoint of a service is the normal case, so a
// single Service annotation fans out to all of them. Per-endpoint variation
// would need the pod-label mechanism built for P/D roles, and nothing asks for
// it yet.
func getEndpointProbe(svc *corev1.Service) (api.EndpointProbe, error) {
	probe := api.EndpointProbe{
		HTTPMethod:    svc.Annotations[probeMethodAnnotation],
		URLPath:       svc.Annotations[probePathAnnotation],
		ExpectedCodes: svc.Annotations[probeExpectedCodesAnnotation],
		HTTPVersion:   svc.Annotations[probeHTTPVersionAnnotation],
		DomainName:    svc.Annotations[probeDomainAnnotation],
	}

	if !probe.IsSet() {
		return probe, nil
	}

	// domainName is two features wearing one name, and they arm differently:
	// it is always the TLS SNI for an HTTPS monitor, but it only becomes the
	// Host header at httpVersion 1.1. Someone who sets a domain for
	// virtual-host health checks and leaves the version alone gets SNI only,
	// with no error and no warning. Default the version instead, which is what
	// they meant; setting it explicitly to 1.0 still wins.
	if probe.DomainName != "" && probe.HTTPVersion == "" {
		probe.HTTPVersion = "1.1"
		klog.V(4).Infof("service %s/%s: %s implies %s=1.1 so the domain is sent as the Host header",
			svc.Namespace, svc.Name, probeDomainAnnotation, probeHTTPVersionAnnotation)
	}

	if err := probe.Validate(); err != nil {
		return api.EndpointProbe{}, err
	}

	return probe, nil
}

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
	pdDisaggAnnotation, pdPrefillSelectorAnnotation, pdDecodeSelectorAnnotation,
	pdPrefillNixlPortAnnotation, pdDecodeNixlPortAnnotation,
	pdCacheAwareAnnotation, pdCacheThresholdAnnotation, pdSessionTTLAnnotation,
	pdBalanceAbsThresholdAnnotation,
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

	aiArgs.PDDisaggMode = boolVal(pdDisaggAnnotation)
	aiArgs.PDCacheAwareMode = boolVal(pdCacheAwareAnnotation)
	aiArgs.PDCacheThreshold = int32(intVal(pdCacheThresholdAnnotation))
	aiArgs.PDSessionTTLSec = int32(intVal(pdSessionTTLAnnotation))
	aiArgs.PDBalanceAbsThreshold = int32(intVal(pdBalanceAbsThresholdAnnotation))

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

	// Endpoints are not known yet, so only the service-level rules can run here.
	// The endpoint-role rules are checked against the real payload in
	// makeLoxiLoadBalancerModel.
	if err := aiArgs.ValidateServiceArgs(mode); err != nil {
		return api.AIArgs{}, err
	}

	if aiArgs.PDDisaggMode {
		if svc.Annotations[pdPrefillSelectorAnnotation] == "" || svc.Annotations[pdDecodeSelectorAnnotation] == "" {
			return api.AIArgs{}, fmt.Errorf("%s requires both %s and %s",
				pdDisaggAnnotation, pdPrefillSelectorAnnotation, pdDecodeSelectorAnnotation)
		}
		if _, err := parsePDPools(svc); err != nil {
			return api.AIArgs{}, err
		}
	}

	return aiArgs, nil
}

// pdPool - one side of a disaggregated pair: which pods belong to it, and the
// NIXL side-channel port they listen on.
type pdPool struct {
	role     int32
	selector labels.Selector
	nixlPort int32
}

// parsePDPools - read the prefill and decode pool definitions from annotations.
func parsePDPools(svc *corev1.Service) ([]pdPool, error) {
	pools := []pdPool{
		{role: api.EpRolePrefill},
		{role: api.EpRoleDecode},
	}
	selAnnotations := []string{pdPrefillSelectorAnnotation, pdDecodeSelectorAnnotation}
	portAnnotations := []string{pdPrefillNixlPortAnnotation, pdDecodeNixlPortAnnotation}

	for i := range pools {
		sel, err := labels.Parse(svc.Annotations[selAnnotations[i]])
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a label selector: %v",
				selAnnotations[i], svc.Annotations[selAnnotations[i]], err)
		}
		pools[i].selector = sel

		port, err := annoInt(svc, portAnnotations[i])
		if err != nil {
			return nil, err
		}
		if port < 0 || port > 65535 {
			return nil, fmt.Errorf("%s must be within 0..65535, got %d", portAnnotations[i], port)
		}
		pools[i].nixlPort = int32(port)
	}

	return pools, nil
}

// pdEndpointRole - what to stamp onto an endpoint whose address belongs to a
// pod of one of the pools.
type pdEndpointRole struct {
	Role     int32
	NixlPort int32
}

// resolvePDRoles - map endpoint address to prefill/decode role from the labels
// of the pods each selector matches.
//
// Returns nil when the service does not use disaggregation, which leaves every
// endpoint at ep_role=0 and keeps the payload unchanged. That early return is
// also what keeps the pod watch lazy: a cluster with no disaggregated service
// never reaches the watcher.
func (m *Manager) resolvePDRoles(ctx context.Context, svc *corev1.Service, aiArgs api.AIArgs) (map[string]pdEndpointRole, error) {
	if !aiArgs.PDDisaggMode {
		return nil, nil
	}

	pools, err := parsePDPools(svc)
	if err != nil {
		return nil, err
	}

	podLister, err := m.pdPods.Lister(ctx, svc.Namespace, podCacheScope(pools))
	if err != nil {
		return nil, err
	}

	roles := make(map[string]pdEndpointRole)
	for _, pool := range pools {
		pods, err := podLister.Pods(svc.Namespace).List(pool.selector)
		if err != nil {
			return nil, fmt.Errorf("failed to select pods for the %s pool: %v", roleName(pool.role), err)
		}

		for _, pod := range pods {
			for _, podIP := range podIPs(pod) {
				if existing, dup := roles[podIP]; dup && existing.Role != pool.role {
					// One pod matching both selectors would make the rule
					// depend on map iteration order. Refuse instead.
					return nil, fmt.Errorf("pod %s/%s (%s) matches both the prefill and the decode selector",
						pod.Namespace, pod.Name, podIP)
				}
				roles[podIP] = pdEndpointRole{Role: pool.role, NixlPort: pool.nixlPort}
			}
		}
	}

	return roles, nil
}

// podIPs - every address a pod answers on, covering dual-stack.
func podIPs(pod *corev1.Pod) []string {
	var ips []string

	if pod.Status.PodIP != "" {
		ips = append(ips, pod.Status.PodIP)
	}
	for _, ip := range pod.Status.PodIPs {
		if ip.IP != "" && ip.IP != pod.Status.PodIP {
			ips = append(ips, ip.IP)
		}
	}

	return ips
}

func roleName(role int32) string {
	if role == api.EpRolePrefill {
		return "prefill"
	}
	return "decode"
}

// ErrInferenceGatewayRequired - the peer cannot serve the requested routing.
// Sentinel so the fan-out can tell this refusal apart from a transport error
// and report it on the Service.
var ErrInferenceGatewayRequired = errors.New("loxilb peer is not a loxilb-inference-gateway")

// ErrGPUMonitoringDisabled - the peer would accept a sel=gpuaware rule and then
// select as CHWBL, because GPU-aware routing is armed instance-wide and is off.
var ErrGPUMonitoringDisabled = errors.New("GPU monitoring is disabled on the loxilb peer")

// checkGPUAware - refuse a sel=gpuaware rule that the peer would silently
// downgrade to CHWBL.
//
// GPU-aware selection has three stages and kube-loxilb only drives the third.
// Stage 1 arms the instance-wide routing mode; stage 2 feeds per-endpoint GPU
// telemetry, which comes from the serving engine or DCGM rather than from the
// Kubernetes API. Neither belongs to kube-loxilb - but unlike the tokenizer,
// whose state cannot be observed at all, stage 1 reports itself. A check that is
// available and not made is a different thing from blindness.
func (m *Manager) checkGPUAware(ctx context.Context, c *api.LoxiClient) error {
	status, readable := m.gpuStatus(ctx, c)
	if !readable {
		// A failed diagnostic must not take down a rule that would have worked.
		return nil
	}

	if !status.GPUArmed() {
		return fmt.Errorf("loxilb-lb(%s) has GPU-aware routing disarmed (enabled=%v, routing_mode=%q), "+
			"so a %s=gpuaware rule would select as CHWBL instead: %w",
			c.Host, status.Enabled, status.RoutingMode, endPointSelAnnotation, ErrGPUMonitoringDisabled)
	}

	if status.WorkerCount == 0 {
		// Armed, but nothing has reported metrics yet, so the selector has no
		// telemetry to act on. Not a refusal: the feed may simply not have run.
		klog.Warningf("loxilb-lb(%s): GPU monitoring is armed but tracking 0 workers - "+
			"GPU-aware selection has no telemetry until worker metrics are pushed to it", c.Host)
	}

	return nil
}

// reportGPUDisarmed - re-check GPU arming on the reconcile path.
//
// The pre-flight in installLB only runs when a rule is programmed, so it cannot
// see an operator disarming the mode afterwards: the rule stops being
// GPU-aware, nothing about the rule changed, and no event is raised. Same shape
// as a pod relabelled in place, and the same fix - look on the loop that runs
// anyway rather than only at the write.
//
// This reports rather than refuses. The rule already exists and is serving; the
// mode being off is not something reprogramming would repair.
func (m *Manager) reportGPUDisarmed(svc *corev1.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	for _, c := range m.LoxiClients.Clients {
		if !c.IsInferenceGateway() {
			continue
		}

		status, readable := m.gpuStatus(ctx, c)
		if !readable {
			// "Cannot tell" is not "disarmed". Leave the remembered state
			// alone so a blip does not read as a transition either way.
			continue
		}

		if !m.gpuArmingChangedToOff(c.Host, status.GPUArmed()) {
			continue
		}

		msg := fmt.Sprintf("loxilb-lb(%s) no longer has GPU-aware routing armed (enabled=%v, routing_mode=%q), "+
			"so this rule now selects as CHWBL",
			c.Host, status.Enabled, status.RoutingMode)
		klog.Errorf("service %s/%s: %s", svc.Namespace, svc.Name, msg)
		m.recordServiceWarning(svc, ReasonGPUMonitoringDisabled, msg)
	}
}

// gpuArmingChangedToOff - remember the arming state per peer and report only
// the edge into disarmed.
//
// A warning on every reconcile is noise even with event aggregation; a warning
// at the moment arming is lost is something an operator can act on. A peer seen
// disarmed for the first time also reports, since that covers a controller
// restarting after the mode was turned off.
func (m *Manager) gpuArmingChangedToOff(host string, armed bool) bool {
	m.gpuArmedMu.Lock()
	defer m.gpuArmedMu.Unlock()

	previous, seen := m.gpuArmed[host]
	m.gpuArmed[host] = armed

	return !armed && (!seen || previous)
}

// gpuStatus - read GPU monitoring state, distinguishing "disarmed" from
// "cannot tell". The difference matters: an unreachable peer must neither
// block a rule nor be reported as a regression.
func (m *Manager) gpuStatus(ctx context.Context, c *api.LoxiClient) (*api.GPUStatusModel, bool) {
	status, err := c.GPU().Status(ctx)
	if err != nil {
		klog.V(4).Infof("loxilb-lb(%s): GPU monitoring status unreadable: %v", c.Host, err)
		return nil, false
	}

	return status, true
}

// Event reasons raised against a Service.
const (
	// ReasonInvalidInferenceConfig - the inference annotations are malformed or
	// describe a combination loxilb rejects.
	ReasonInvalidInferenceConfig = "InvalidInferenceConfig"
	// ReasonInferenceGatewayRequired - the service asks for inference routing
	// but the loxilb it would be programmed into is plain upstream loxilb.
	ReasonInferenceGatewayRequired = "InferenceGatewayRequired"
	// ReasonKvExactTokenizerRequired - the rule uses KV-exact routing, which
	// needs a tokenizer staged inside loxilb that kube-loxilb cannot see.
	ReasonKvExactTokenizerRequired = "KvExactTokenizerRequired"
	// ReasonGPUMonitoringDisabled - the rule selects GPU-aware routing on a
	// loxilb instance where that mode is not armed.
	ReasonGPUMonitoringDisabled = "GPUMonitoringDisabled"
	// ReasonInvalidProbeConfig - the health-monitor annotations are malformed.
	ReasonInvalidProbeConfig = "InvalidProbeConfig"
	// ReasonProbeModeChanged - the probe annotations retire a proberesp check
	// that is still configured.
	ReasonProbeModeChanged = "ProbeModeChanged"
	// ReasonProbeFieldsDowngraded - a plain upstream peer in the pool cannot
	// honour part of the probe configuration.
	ReasonProbeFieldsDowngraded = "ProbeFieldsDowngraded"
	// ReasonInvalidGatewayArgs - a gateway-only service annotation is malformed.
	ReasonInvalidGatewayArgs = "InvalidGatewayArgs"
	// ReasonGatewayArgsDowngraded - a plain upstream peer in the pool will not
	// apply part of the gateway-only service configuration.
	ReasonGatewayArgsDowngraded = "GatewayArgsDowngraded"
	// ReasonRuleMissing - a peer no longer has a rule kube-loxilb programmed.
	ReasonRuleMissing = "RuleMissing"
	// ReasonRuleUnhealthy - loxilb reports the rule as degraded or offline.
	ReasonRuleUnhealthy = "RuleUnhealthy"
	// ReasonRuleHealthy - the rule recovered.
	ReasonRuleHealthy = "RuleHealthy"
	// ReasonLoxiLBRejected - loxilb refused the rule and said why. Carries
	// loxilb's own wording, since it is more specific than anything
	// kube-loxilb could reconstruct.
	ReasonLoxiLBRejected = "LoxiLBRejected"
)

// recordServiceEvent - surface something on the Service itself, so
// `kubectl describe svc` explains it. A log line on the agent does not reach
// the person who wrote the annotation.
func (m *Manager) recordServiceEvent(svc *corev1.Service, eventType, reason, message string) {
	if m.eventRecorder == nil || svc == nil {
		return
	}

	m.eventRecorder.Event(svc, eventType, reason, message)
}

// recordServiceWarning - report a rejected configuration.
func (m *Manager) recordServiceWarning(svc *corev1.Service, reason, message string) {
	m.recordServiceEvent(svc, corev1.EventTypeWarning, reason, message)
}

// kvTokenizerDir - where the gateway looks for tokenizers
// (kvTokenizerDir in its pkg/loxinet/ai_kv_router.go).
const kvTokenizerDir = "/etc/loxilb/tokenizers"

// kvModelSlug - the gateway's filesystem name for a model: every "/" becomes
// "__" and nothing else changes. Mirrors kvModelSlug in ai_kv_router.go.
func kvModelSlug(modelName string) string {
	return strings.ReplaceAll(modelName, "/", "__")
}

// tokenizerNotice - what an operator has to check when a rule turns on KV-exact
// routing, or "" when the rule does not use it.
//
// This is deliberately a notice and not a readiness signal. kube-loxilb cannot
// verify any of it: the file lives inside the loxilb pod, and the gateway loads
// it lazily on the first request for a model rather than when the rule is
// created, so a rule that programs cleanly proves nothing. Reporting health
// that was never checked would be worse than saying nothing. Naming the exact
// path is the most that can honestly be said.
//
// The stakes are why it is said at all: a missing tokenizer does not fail the
// rule, it silently downgrades KV-exact routing to load-based routing. The
// gateway logs once per model and caches the failure, so staging the file
// afterwards does not take effect until loxilb restarts.
func tokenizerNotice(aiArgs api.AIArgs) string {
	if aiArgs.KvExactMode == api.KvExactModeOff {
		return ""
	}

	target := fmt.Sprintf("%s/%s/tokenizer.json", kvTokenizerDir, kvModelSlug(aiArgs.ModelName))
	if aiArgs.ModelName == "" {
		target = fmt.Sprintf("%s/<model>/tokenizer.json for every model this rule serves, where <model> is the requested model name with each \"/\" replaced by \"__\" (%s is unset, so the model comes from each request)",
			kvTokenizerDir, modelNameAnnotation)
	}

	return fmt.Sprintf("KV-exact routing is enabled (%s=%d): loxilb must already have a tokenizer staged at %s. "+
		"kube-loxilb cannot check this. If it is missing, loxilb logs \"kv-router: tokenizer not available\" once and "+
		"falls back to load-based routing - the rule is still created and traffic still flows, just without cache-aware "+
		"placement. loxilb caches that failure, so staging the file afterwards needs a loxilb restart to take effect.",
		kvExactModeAnnotation, aiArgs.KvExactMode, target)
}

// recordTokenizerNotice - emit the notice above when a rule is programmed.
//
// Normal, not Warning: nothing has been detected as wrong, and a Warning that
// fires on every correctly configured KV-exact service would devalue the
// Warnings that report real rejections.
func (m *Manager) recordTokenizerNotice(svc *corev1.Service, aiArgs api.AIArgs) {
	notice := tokenizerNotice(aiArgs)
	if notice == "" {
		return
	}

	klog.Infof("service %s/%s: %s", svc.Namespace, svc.Name, notice)
	m.recordServiceEvent(svc, corev1.EventTypeNormal, ReasonKvExactTokenizerRequired, notice)
}

// recordProbeNotices - report the two things about a probe configuration that
// the annotations do not show on their own.
func (m *Manager) recordProbeNotices(svc *corev1.Service, probe api.EndpointProbe) {
	if !probe.IsSet() {
		return
	}

	if notice := probeModeNotice(svc, probe); notice != "" {
		klog.Warningf("service %s/%s: %s", svc.Namespace, svc.Name, notice)
		m.recordServiceWarning(svc, ReasonProbeModeChanged, notice)
	}

	if notice := probeDowngradeNotice(m.plainLoxilbPeers(), probe); notice != "" {
		klog.Infof("service %s/%s: %s", svc.Namespace, svc.Name, notice)
		m.recordServiceEvent(svc, corev1.EventTypeNormal, ReasonProbeFieldsDowngraded, notice)
	}
}

// probeModeNotice - warn when the structured fields retire a proberesp check
// the operator did not touch.
//
// The five fields are not additive refinements of the legacy probe; any one of
// them switches the prober into structured mode, where the response check
// becomes a status-code match and proberesp stops being consulted. Someone
// running proberesp: "OK" who adds probe-domain purely to fix SNI loses the
// body check, and nothing in the annotation they edited suggests that.
func probeModeNotice(svc *corev1.Service, probe api.EndpointProbe) string {
	if svc.Annotations[probeRespAnnotation] == "" || !probe.SwitchesProberMode() {
		return ""
	}

	return fmt.Sprintf("%s is set but will no longer be checked: the probe annotations switch loxilb to "+
		"status-code matching, so the probe now passes on HTTP %s instead of on the response containing %q. "+
		"Remove the probe-* annotations to keep the body check",
		probeRespAnnotation, probe.EffectiveExpectedCodes(), svc.Annotations[probeRespAnnotation])
}

// probeDowngradeNotice - say which probe settings a plain upstream peer in the
// pool cannot honour.
//
// The path itself survives: it is carried across to probereq, which upstream
// formats into the probe URL. The rest have no upstream equivalent, so on those
// peers the probe keeps the older behaviour.
func probeDowngradeNotice(plainPeers []string, probe api.EndpointProbe) string {
	if len(plainPeers) == 0 {
		return ""
	}

	var dropped []string
	if probe.HTTPMethod != "" {
		dropped = append(dropped, probeMethodAnnotation)
	}
	if probe.ExpectedCodes != "" {
		dropped = append(dropped, probeExpectedCodesAnnotation)
	}
	if probe.HTTPVersion != "" {
		dropped = append(dropped, probeHTTPVersionAnnotation)
	}
	if probe.DomainName != "" {
		dropped = append(dropped, probeDomainAnnotation)
	}
	if len(dropped) == 0 {
		return ""
	}

	return fmt.Sprintf("%s have no equivalent in plain upstream loxilb and are not applied on %s; "+
		"the probe path is still honoured there, carried as %s",
		strings.Join(dropped, ", "), strings.Join(plainPeers, ", "), probeReqAnnotation)
}

// plainLoxilbPeers - peers known to be plain upstream loxilb. Undetected peers
// are excluded: reporting a downgrade before the flavor is known would be a
// guess.
func (m *Manager) plainLoxilbPeers() []string {
	var plain []string

	for _, c := range m.LoxiClients.Clients {
		if c.FlavorDetected() && !c.IsInferenceGateway() {
			plain = append(plain, c.Host)
		}
	}

	return plain
}
