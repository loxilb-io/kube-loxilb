# Inference gateway guide

kube-loxilb can program [loxilb-inference-gateway](https://github.com/loxilb-io/loxilb-inference-gateway)
as an LLM-serving load balancer: prefix-cache aware endpoint selection, KV-cache exact routing,
model-name routing, SSE-aware streaming, and prefill/decode disaggregation. This document covers how
to use those features and every annotation that drives them.

There are two ways in, and they end up in the same place - a LoadBalancer Service that kube-loxilb
turns into an inference rule on loxilb:

| | [Through a Kubernetes Service](#1-through-a-kubernetes-service) | [Through the Gateway API](#2-through-the-gateway-api-inferencepool) |
|---|---|---|
| What you write | a `LoadBalancer` Service with `loxilb.io/*` annotations | an `InferencePool` + an `HTTPRoute` that references it |
| kube-loxilb flags | none beyond the usual | `--gatewayAPI --inferenceExtension` |
| Extra CRDs | none | Gateway API + Inference Extension |
| Endpoints | the Service selector | the pool's `selector` |
| Everything below the Service | identical | identical |

Use the Service path when loxilb is the only thing routing to your model servers. Use the Gateway API
path when the fleet is already described with Gateway API objects, or when
[Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) semantics matter to other
tooling in the cluster.

## Contents

- [Before you start](#before-you-start)
- [1. Through a Kubernetes Service](#1-through-a-kubernetes-service)
  - [Prefill/decode disaggregation](#prefilldecode-disaggregation)
- [2. Through the Gateway API (InferencePool)](#2-through-the-gateway-api-inferencepool)
- [Annotation reference](#annotation-reference)
  - [Inference gateway annotations](#inference-gateway-annotations)
  - [Prefill/decode annotations](#prefilldecode-annotations)
  - [Endpoint selection](#endpoint-selection)
  - [Annotations that are not inference-specific](#annotations-that-are-not-inference-specific)
- [Requirements outside Kubernetes](#requirements-outside-kubernetes)
- [Events and status](#events-and-status)

## Before you start

**A loxilb-inference-gateway backend.** Every annotation on this page is served by
loxilb-inference-gateway, a superset of upstream loxilb aimed at LLM serving fleets. kube-loxilb
detects the flavor at runtime from the `product` field of `GET /netlox/v1/version`, so the same
kube-loxilb drives both. A service that asks for any of these annotations while its loxilb is plain
upstream loxilb is rejected with an `InferenceGatewayRequired` Warning event on the Service rather
than being silently downgraded.

**fullproxy and pod endpoints.** Endpoint selection and the KV-exact paths run in the userspace
proxy, which only fullproxy traffic reaches, so these settings need
<b>loxilb.io/lbmode: "fullproxy"</b> in practice. Selecting on cache locality, GPU state or a
prefill/decode role also means choosing between individual model server pods, so the endpoints have
to be pods - set <b>loxilb.io/usepodnetwork: "yes"</b> (the Gateway API path imposes both for you).
Omitting an annotation leaves the field out of the request entirely, so loxilb applies its own default.

## 1. Through a Kubernetes Service

Annotate an ordinary `LoadBalancer` Service whose selector picks up the model server pods. Nothing
else is needed: no CRDs, no extra kube-loxilb flags.

Example - prefix-cache aware routing across a vLLM pool:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: vllm-llama70b
  annotations:
    loxilb.io/lbmode: "fullproxy"
    loxilb.io/epselect: "chwbl"
    loxilb.io/chwbl-prefix-hash-level: "2"
    loxilb.io/chwbl-mean-load-factor: "125"
    loxilb.io/sse-mode: "true"
    loxilb.io/model-name: "meta-llama/Llama-3.1-70B-Instruct"
spec:
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app: vllm
  ports:
    - port: 8000
      targetPort: 8000
      protocol: TCP
  type: LoadBalancer
```

That service asks for four things at once: `chwbl` picks the endpoint whose cache most likely already
holds the prompt prefix, `chwbl-*` tunes how that hashing works, `sse-mode` keeps a streaming response
from being torn down by the idle timeout, and `model-name` lets other services share the same VIP and
port by claiming a different model. Each is described in
[Inference gateway annotations](#inference-gateway-annotations).

`chwbl` is one of three selectors that exist only on the inference gateway - see
[Endpoint selection](#endpoint-selection) for the rest, and for what `gpuaware` additionally needs.

Verify the rule the way you would any other loxilb service:

```
kubectl get svc vllm-llama70b
kubectl describe svc vllm-llama70b     # events report anything loxilb refused
```

### Prefill/decode disaggregation

Disaggregation needs a role per endpoint, which a Service cannot state directly. kube-loxilb derives it: two label selectors name the prefill and the decode pods, and each endpoint is stamped with the role of the pod it belongs to.

Because the role is per pod, the endpoints have to <b>be</b> pods. Set <b>loxilb.io/usepodnetwork: "yes"</b> (or use a multus network). In the default mode the endpoints are node addresses, every pod on a node collapses into one entry, and the split cannot be represented - kube-loxilb rejects that combination rather than programming a rule that cannot work.

The annotations are listed under [Prefill/decode annotations](#prefilldecode-annotations).

With disaggregation on, <b>loxilb.io/kv-exact-mode: "1"</b> becomes available; mode `3` is for a
single role-less pool and loxilb rejects it here.

The port is uniform per pool, not per pod: all prefill pods are assumed to share one NIXL port and
all decode pods another, which is how one Deployment per role deploys. Per-pod ports would need a pod
annotation and are not supported.

Example - a prefill pool and a decode pool behind one Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: vllm-pd
  annotations:
    loxilb.io/lbmode: "fullproxy"
    loxilb.io/usepodnetwork: "yes"
    loxilb.io/pd-disagg: "true"
    loxilb.io/pd-prefill-selector: "llm-role=prefill"
    loxilb.io/pd-decode-selector: "llm-role=decode"
    loxilb.io/pd-prefill-nixl-port: "9001"
    loxilb.io/pd-decode-nixl-port: "9002"
    loxilb.io/kv-exact-mode: "1"
    loxilb.io/sse-mode: "true"
spec:
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app: vllm          # selects both pools
  ports:
    - port: 8000
      targetPort: 8000
      protocol: TCP
  type: LoadBalancer
```

The Service selector must cover both pools, since one rule carries both. The two role selectors then partition what it found.

## 2. Through the Gateway API (InferencePool)

Start kube-loxilb with `--inferenceExtension` (which needs `--gatewayAPI`) to serve
[Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) pools.
An `InferencePool` referenced from an HTTPRoute becomes a LoadBalancer Service selecting the pool's
pods, which loxilb then programs as an inference rule - the same rule the
[Service path](#1-through-a-kubernetes-service) produces, written differently.

The GatewayClass and Gateway themselves are ordinary Gateway API objects - see
[Gateway API support](README.md#gateway-api-support) in the README for those. What is added here is
the pool and the route that points at it:

```yaml
apiVersion: inference.networking.k8s.io/v1
kind: InferencePool
metadata:
  name: vllm-qwen3
  namespace: llm
  annotations:
    loxilb.io/epselect: "chwbl"           # prefix-cache aware endpoint selection
    loxilb.io/model-name: "qwen3-32b"
spec:
  selector:
    matchLabels:
      app: vllm-qwen3
  targetPorts:
    - number: 8000
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: llm
spec:
  parentRefs:
    - name: inference-gw
  rules:
    - backendRefs:
        - group: inference.networking.k8s.io
          kind: InferencePool
          name: vllm-qwen3
```

The generated Service is named `<pool>-inference`, sits on the Gateway's address, and selects the
pool's pods. What it carries and what the translation decides for you is below.

**Endpoint selection is loxilb's.** The extension's reference design delegates it to an Endpoint
Picker over ext-proc; loxilb-inference-gateway selects endpoints in the data plane instead, so a
pool's `endpointPickerRef` is not called. A pool that sets it with `failureMode: FailOpen` is
accepted and the picker ignored; with `FailClose` - the API default - the pool is refused, and
`status.parents[].conditions` says why rather than routing by a policy you did not ask for.

**Model routing needs a lookup key.** The gateway's userspace proxy finds an endpoint pool by
host, path prefix and match mode, so a rule carrying inference settings is given `host` = its
external IP, `path_prefix` = `/` and `path_match_mode` = `prefix`. Without them the rule is
accepted, reads back correctly, and answers every request with `model_unavailable`.

**The pool takes the listener.** A Gateway with HTTP/HTTPS listeners normally also gets a
`<gateway>-ingress-service` pointing at the loxilb-ingress pods. A listener that an InferencePool
is attached to is left out of that Service - both would otherwise claim the same address and port,
and which rule the data plane binds would be a race.

**CRDs.** Install the Inference Extension CRDs separately; kube-loxilb waits for
`inferencepools.inference.networking.k8s.io` and does nothing until it exists. Pools that omit
`endpointPickerRef` need the **v1.6.0** CRDs or later - v1.5.0 and earlier make the field required.

**Where the annotations come from.** Only `loxilb.io/*` annotations are copied - first the pool's,
then the route's, so the same pool served through two routes can be tuned per route. Everything in
[Annotation reference](#annotation-reference) can be set in either place.

**What the translation imposes.** `loxilb.io/usepodnetwork` is forced to `"yes"` - inference routing
chooses between individual model server pods, which node-address endpoints cannot express - and
setting it to anything else is an error rather than a silent override. `loxilb.io/lbmode` defaults to `fullproxy`;
if the pool or route sets it to something else while asking for a gateway-only selector
(`chwbl`, `gpuaware`, `wrr-hash`), the pool is refused - outside fullproxy that selector matches no
case and the traffic is black-holed.

**Ports.** The Service listens on the Gateway listener's port and targets the pool's first
`targetPorts` entry. A pool declaring more than one target port is served on the first, with a
warning in the kube-loxilb log.

**One Service per listener.** A pool reachable through several Gateway listeners gets one Service per
listener, named `<pool>-inference-<gateway>-<listener>`; long names are truncated to the API server's
63-character limit without letting two of them collapse onto one.

For a worked, runnable version of this path - k3s, a real gateway container, and the assertions that
prove the rule carries traffic - see [test/e2e-inference-gwapi](test/e2e-inference-gwapi/README.md).

## Annotation reference

Everything here can be set on a Service, or on an `InferencePool` / `HTTPRoute` when using the
Gateway API path. These are the annotations added for inference serving; the rest of kube-loxilb's
annotations are in the [README](README.md#kube-loxilb-supported-annotations).

### Inference gateway annotations

Model routing, streaming, session affinity, prefix-cache hashing and KV-exact routing. All of these
need <b>loxilb.io/lbmode: "fullproxy"</b> and a loxilb-inference-gateway peer.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/model-name</b> | Route by the `model` field of the request body. Several services can share one VIP:port, each claiming a model name; a service with no model name is the catch-all. |
| <b>loxilb.io/sse-mode</b> | Server-sent-events awareness: suppresses the idle timeout during token streaming. Also arms AI key and rate-limit enforcement. `"true"`/`"yes"`. |
| <b>loxilb.io/max-stream-duration</b> | Ceiling in seconds for a single streamed response. Default 0 (unbounded). |
| <b>loxilb.io/backend-keepalive-interval</b> | Backend keepalive interval in seconds. Default 0. |
| <b>loxilb.io/session-header-name</b> | Pin a session to one endpoint by an HTTP header, e.g. `"mcp-session-id"` for an MCP gateway. |
| <b>loxilb.io/trace-type</b> | Protocol-aware tracing, e.g. `"mcp"`. |
| <b>loxilb.io/cb-enable</b> | Per-endpoint circuit breaker. `"true"`/`"yes"`. |
| <b>loxilb.io/chwbl-prefix-hash-level</b> | Prefix-hash depth for consistent hashing with bounded loads: `1`, `2` or `3`. Default 1. Used by `epselect: chwbl` and `epselect: wrr-hash`. |
| <b>loxilb.io/chwbl-prefix-hash-flags</b> | Prefix-hash flags, 0..255. Default 0. |
| <b>loxilb.io/chwbl-mean-load-factor</b> | Bounded-load factor as a percentage, 100..300. Default 125. Lower values spread load more evenly at the cost of cache locality. |
| <b>loxilb.io/chwbl-replication</b> | Virtual nodes per endpoint on the hash ring, 1..1024. Default 100. |
| <b>loxilb.io/chwbl-enable-cache-salt</b> | Salt the cache key. `"true"`/`"yes"`. Default false. |
| <b>loxilb.io/kv-exact-mode</b> | KV-cache exact routing. Use `"3"` for a single role-less serving pool, or `"1"` alongside [prefill/decode disaggregation](#prefilldecode-disaggregation). |
| <b>loxilb.io/kv-engine-type</b> | `"vllm"` (default) or `"sglang"`. Immutable once the rule exists: changing it needs a delete and recreate. |
| <b>loxilb.io/kv-dp-rank-count</b> | SGLang `--dp-size`, 1..8. Default 1. |
| <b>loxilb.io/kv-block-size</b> | KV block size in tokens. Default 16. Must match the engine. |
| <b>loxilb.io/kv-zmq-port</b> | Engine event socket port. Default 5557. |
| <b>loxilb.io/kv-warmup-sec</b> | Warmup window in seconds. The gateway's swagger documents a default of 30 but no code applies it, so set this explicitly if warmup matters. |
| <b>loxilb.io/kv-hash-algo</b> | `"sha256_cbor"`, `"xxhash_cbor"` or `"sha256_sglang"`. Best omitted: loxilb then derives it from the engine type and the pair can never be incoherent. |

### Prefill/decode annotations

See [Prefill/decode disaggregation](#prefilldecode-disaggregation) for how the roles are derived and
what they require.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/pd-disagg</b> | Turn on prefill/decode disaggregation. `"true"`/`"yes"`. Requires fullproxy, pod endpoints, and both selectors below. |
| <b>loxilb.io/pd-prefill-selector</b> | Label selector for the prefill pods, e.g. `"llm-role=prefill"`. |
| <b>loxilb.io/pd-decode-selector</b> | Label selector for the decode pods. A pod may not match both. |
| <b>loxilb.io/pd-prefill-nixl-port</b> | NIXL side-channel port the prefill pods listen on, matching their `VLLM_NIXL_SIDE_CHANNEL_PORT`. 0 or unset reuses the target port. |
| <b>loxilb.io/pd-decode-nixl-port</b> | The same for the decode pods. |
| <b>loxilb.io/pd-cache-aware</b> | Cache-aware prefill placement. Requires `pd-disagg`. |
| <b>loxilb.io/pd-cache-threshold</b> | Cache-hit percentage above which the cached prefill endpoint is preferred, 0..100. Default 20. |
| <b>loxilb.io/pd-session-ttl</b> | Session lifetime in seconds. Default 0. |
| <b>loxilb.io/pd-balance-abs-threshold</b> | Absolute load gap before rebalancing. Default 3. |

### Endpoint selection

`loxilb.io/epselect` is not new, but three of its values are: `chwbl`, `gpuaware` and `wrr-hash` exist
only in loxilb-inference-gateway and only work under `loxilb.io/lbmode: "fullproxy"`. Outside
fullproxy the kernel selector matches no case and black-holes the traffic, so kube-loxilb refuses that
combination rather than programming it.

| Value | What it does | Also needs |
| ----- | ------------ | ---------- |
| `chwbl` | Consistent hashing with bounded loads - routes a prompt to the endpoint whose cache most likely holds its prefix. | the `chwbl-*` annotations to tune it |
| `wrr-hash` | Weighted round-robin over the same prefix hash. | the `chwbl-*` annotations |
| `gpuaware` | Selects on live GPU telemetry. | [arming on each loxilb instance](#gpu-aware-routing-has-to-be-armed-outside-kubernetes) |

The other values (`rr`, `hash`, `persist`, `lc`, ...) work against any loxilb - see the
[README](README.md#kube-loxilb-supported-annotations).

### Annotations that are not inference-specific

Three more annotation families were added alongside these. They are documented in the README because
they apply to any service, not only to model serving, but they are worth knowing about here because
they exist only in loxilb-inference-gateway too:

- [**Endpoint health-monitor**](README.md#endpoint-health-monitor-annotations)
  (`loxilb.io/probe-method`, `probe-path`, `probe-expected-codes`, `probe-http-version`,
  `probe-domain`) - a real HTTP health check instead of the `probereq`/`proberesp` escape hatch.
  Setting any of them replaces the legacy probe rather than refining it.
- [**Gateway-only service settings**](README.md#gateway-only-service-annotations)
  (`loxilb.io/connection-limit`, the `timeout-*`, `tls-*`, `hsts-*` and `backend-*-cert-id`
  annotations) - per-service limits, member timeouts and TLS/HSTS policy. A useful companion to a
  public inference endpoint.
- [**Rule health on the Service**](README.md#rule-health-on-the-service) - what loxilb says about a
  rule, surfaced on `Service.status` and as events. Needs `loxilb.io/liveness: "yes"`.
- **mTLS** (`loxilb.io/mtls-*`) - frontend and backend mutual TLS for the endpoint in front of the
  pool.

## Requirements outside Kubernetes

Two of these features depend on state kube-loxilb cannot create from the Kubernetes API.

### GPU-aware routing has to be armed outside Kubernetes

`loxilb.io/epselect: "gpuaware"` sets the rule's selector, but whether that selector actually runs is decided by a process-global routing mode on each loxilb instance, and it is off by default. A rule created against a loxilb with it off is accepted and then routes as plain CHWBL, indistinguishable from `epselect: "chwbl"`.

Two things have to happen on the loxilb side, and neither is something kube-loxilb can do:

1. `POST /netlox/v1/config/gpu/enable` on each instance, which arms the routing mode.
2. Per-endpoint GPU telemetry pushed to `POST /netlox/v1/config/worker/metrics`. This comes from the serving engine or DCGM; without it the selector is armed but has no data.

kube-loxilb does not drive either: the first is one instance-wide switch with no reference counting, and the second needs metrics the Kubernetes API does not have. What it does do is check. Before programming a `gpuaware` rule it reads `GET /netlox/v1/config/gpu/status`, and if the mode is disarmed it refuses the rule on that instance with a `GPUMonitoringDisabled` warning event rather than letting it silently become CHWBL. The same check repeats on every reconcile of a `gpuaware` service, so disarming the mode after the rule exists is reported too. If the status cannot be read at all, the rule is programmed anyway -- a failed diagnostic should not take down a rule that would have worked.

### KV-exact routing needs a staged tokenizer

loxilb reads `/etc/loxilb/tokenizers/<model-slug>/tokenizer.json`, where `<model-slug>` is the model name with each `/` replaced by `__`. kube-loxilb does not manage that file and cannot see it, so whenever a rule enables `kv-exact-mode` it records a Normal `KvExactTokenizerRequired` event on the Service naming the exact path to check -- visible with `kubectl describe svc`.

If the file is missing, loxilb logs `kv-router: tokenizer not available` once and silently falls back to load-based routing: the rule is still created and traffic still flows, just without cache-aware placement. <b>loxilb caches that failure</b>, so staging the tokenizer afterwards does not take effect until loxilb restarts. Stage it before creating the rule.

## Events and status

Everything kube-loxilb refuses, downgrades or is told by loxilb lands on the object as an event, so
`kubectl describe` is the first place to look:

| Reason | Type | Meaning |
| ------ | ---- | ------- |
| `InferenceGatewayRequired` | Warning | An inference annotation was used against plain upstream loxilb. The rule is refused rather than silently downgraded. |
| `InvalidInferenceConfig` | Warning | An annotation did not parse, or a combination is impossible (e.g. `pd-disagg` without pod endpoints). |
| `GPUMonitoringDisabled` | Warning | `epselect: gpuaware` on an instance where the GPU routing mode is not armed. Re-checked on every reconcile. |
| `KvExactTokenizerRequired` | Normal | `kv-exact-mode` is on; names the tokenizer path loxilb will read. |
| `LoxiLBRejected` | Warning | loxilb itself refused the value - an unknown cipher, an unregistered certificate id, and so on. |
| `GatewayArgsDowngraded` | Warning | A gateway-only service setting was dropped for an upstream peer in the pool; names which. |
| `ProbeFieldsDowngraded` | Warning | Health-monitor fields dropped for an upstream peer; the probe path is carried across to `probereq`. |
| `ProbeModeChanged` | Warning | Both the new health monitor and `proberesp` are configured; names what will actually be checked. |
| `RuleUnhealthy` / `RuleHealthy` | Warning / Normal | loxilb's own view of the rule changed. Needs `loxilb.io/liveness: "yes"`. |

On the Gateway API path the pool also carries `status.parents[].conditions` (`Accepted`,
`ResolvedRefs`) under the Gateway that serves it, which is where a refused `endpointPickerRef` is
reported.

## See also

- [README](README.md) - deploying kube-loxilb, and the full annotation list
- [Gateway API support](README.md#gateway-api-support) - the non-inference Gateway API resources
- [test/e2e-inference-gwapi](test/e2e-inference-gwapi/README.md) - the InferencePool path end to end
- [loxilb-inference-gateway](https://github.com/loxilb-io/loxilb-inference-gateway)
