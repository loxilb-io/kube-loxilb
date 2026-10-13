# e2e-inference-gwapi — InferencePool end to end

Proves the [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/)
path all the way down: an `InferencePool` referenced from an HTTPRoute becomes a LoadBalancer
Service, which becomes an **inference rule on a real loxilb-inference-gateway**, which then serves
a request.

Where [test/wire-compat](../wire-compat/README.md) asks *"did the payload change?"* against a fake
peer, this asks *"does the whole thing work?"* against the real one.

## Topology

```
   k3s node  (model server pods on 10.42.0.0/16)
       │ igwbr 12.12.12.254/24
       │
     igw1   12.12.12.1   loxilb-inference-gateway
                         REST :11111, VIP 12.12.12.100:8080
```

The gateway runs as a container beside the cluster and reaches pods over the host bridge, which is
how the loxilb cicd scenarios model an external load balancer. Endpoints are **pod** addresses -
`usepodnetwork` is imposed by the controller - so the container is given a route back into the
cluster network. Without it the rule is programmed and every endpoint is unreachable.

Model servers are `socat` answering with their own pod name. The banner is the delivery proof:
it identifies which endpoint actually served the request, which metrics alone cannot.

## Requirements

`docker`, `kubectl`, `jq`, `curl`, and passwordless `sudo` (k3s install, bridge, veth).
The host needs to be free of other loxilb cicd runs - the scenarios use fixed container names.

The Endpoint Picker checks (5-6d, including 5c) also need `go`, a loxilb-inference-gateway checkout in `EPP_SRC`
(its `cmd/epp-fake` becomes the stand-in picker), and an `IGW_IMAGE` whose `GET /version` lists
`"epp"` in `features`. Without either, those checks report `[SKIPPED]` and the rest still run.

## Run

```bash
./config.sh          # ~4 min: gateway container, bridge, k3s, CRDs, kube-loxilb, fixtures
EPP_SRC=~/src/loxilb-inference-gateway IGW_IMAGE=<gateway with "epp"> ./config.sh   # with the picker
./validation.sh      # the assertions
./rmconfig.sh        # teardown (PURGE_K3S=1 to remove k3s too)
```

Useful variables: `SKIP_BUILD=1` (reuse the image), `IGW_IMAGE`, `KLB_TAG`, `GIE_VERSION`,
`GWAPI_VERSION`, `VIP_POOL`, `EPP_SRC`, `EPP_IMAGE`, `SVC_CIDR`.

## What is asserted

| # | Check | What it proves |
|---|---|---|
| 1 | `vllm-qwen3-inference` Service exists with the expected annotations, selector and ports | the pool was translated, and `lbmode: fullproxy` / `usepodnetwork: yes` were imposed |
| 2 | it carries the gateway's address | the pool was attached to the Gateway the HTTPRoute names |
| 3 | loxilb has a rule on the VIP with `sel=8`, `mode=4`, `model_name`, endpoints = pod IPs | **the flavor was detected and the gateway-only fields were not stripped** — 8 (CHWBL) and 4 (fullproxy) exist only on the inference gateway |
| 3a | the rule carries `host`, `path_prefix`, `path_match_mode` | the proxy looks a pool up by that key; a rule without it reads back correctly and answers `model_unavailable` to everything |
| 3b | exactly one rule on the VIP:port, and no `<gw>-ingress-service` for that listener | the Gateway's own ingress service yields a listener an InferencePool claims — two rules on one address and port is a race over which the data plane binds |
| 3c | a socket is bound on the VIP:port inside the gateway | `mode=4` is only real if fullproxy bound. A rule that failed to bind reads back over REST exactly like one that worked |
| 4 | `status.parents[]` carries Accepted / ResolvedRefs under the Gateway | the controller reports, and under the right parent |
| 4a | the pool's and its Service's `resourceVersion` stay put for 15 s (up to three tries while the testbed settles) | the controller does not rewrite what has not changed; each status write used to queue the pool again, rewriting both every couple of seconds |
| 4b | a hand-written `loxilb.io/epp-endpoint` on a pool without `endpointPickerRef` reaches neither the Service nor the rule | the picker's address belongs to the spec; an annotation cannot switch one on |
| 5 | a `FailClose` pool naming the `epp` Service: Accepted and ResolvedRefs True, the rule's `eppEndpoint` is that Service's cluster IP, and every request is answered by the one pod the picker names | the reference was resolved to an address the gateway dials, and the gateway follows the picker rather than its own selection |
| 5b | with the picker scaled to 0 the pool answers `503 epp_unavailable`, and follows the picker again once it is back | FailClose is enforced, and recovery needs no reprogramming |
| 5c | with `loxilb.io/epp-timeout-ms: "1000"` on the pool and the picker in `-mode hang`, the rule carries `eppTimeoutMs: 1000` and the 503 comes back after about a second, well under the 3000 ms default | the deadline, which has no API field, reaches the gateway and is the one applied |
| 6 | switched to `FailOpen` with a picker Service that does not exist: ResolvedRefs False / InvalidExtensionRef, no `eppEndpoint` on the rule, requests still served | an unresolvable reference falls back as FailOpen declares |
| 6b | creating that Service puts its address on the rule | the Service watch: nothing about the pool changed |
| 6c | recreating it with a new cluster IP moves the rule to the new address | the address is followed, not cached |
| 6d | switched to `FailClose`, deleting the Service keeps the pool's Service and rule, points the rule's `eppEndpoint` at `127.0.0.1:1`, and requests get `503 epp_unavailable` | an unresolvable reference is not served when FailClose forbids it, and clients get the 5xx the Inference Extension's conformance expects rather than a refused connection |
| 7 | deleting the route deletes the Service **and its loxilb rule** | ownership is tracked and cleaned up; a rule keyed by host and path is not reachable by the L4 tuple alone |
| 8 | a request naming the model is answered by one of the pool's pods, and one naming another model is not | the rule carries traffic, and `model_name` selects rather than decorates |
| 9 | deleting the pool's route leaves no rule on the VIP:port | `model_name` is part of loxilb's rule key and has to be sent on the delete — get it wrong and the rule outlives its Service, still bound and still answering |

Check 3 is the one that would have caught a payload regression on a real peer, and checks 5 and 5b
the ones that would catch the picker being dropped on the way to the gateway. 3a, 3b and 3c all exist because of failures this scenario
found the first time it ran; 7's rule assertion and 9 exist because creating the routing key in 3a
made every inference rule undeletable, and nothing here noticed.

## Notes

- **The VIP is the gateway's own address.** fullproxy binds a socket to it, and an address loxilb
  owns only as a `/32` rule device cannot be bound - `bind failed Cannot assign requested address`,
  after which the rule exists, reads back correctly, and nothing listens. `VIP_POOL` defaults
  accordingly.
- **The banner is the proof.** Model servers answer with their own pod name, so check 8 can say
  which endpoint served. A 200 alone would not separate "routed to a pod" from "answered by the
  proxy".
- **The picker is reached at its cluster IP.** The gateway runs outside the cluster, so config.sh
  routes the service CIDR (`SVC_CIDR`, k3s default `10.43.0.0/16`) to the node, where kube-proxy
  DNATs it - the same route any loxilb outside the cluster needs.
- The stand-in picker serves a self-signed certificate, as the llm-d EPP does, so the gateway's
  default transport (TLS without verification) is what is exercised.
- The Inference Extension CRDs are installed at **v1.6.0**: `endpointPickerRef` is required in
  v1.5.0 and earlier, so the pools here - which omit it - would be rejected by the API server.
- `sel` and `mode` are asserted numerically on purpose. Names are a client-side convenience;
  the number is what goes on the wire and what loxilb acts on.
