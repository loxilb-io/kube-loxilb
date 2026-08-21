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

## Run

```bash
./config.sh          # ~4 min: gateway container, bridge, k3s, CRDs, kube-loxilb, fixtures
./validation.sh      # the assertions
./rmconfig.sh        # teardown (PURGE_K3S=1 to remove k3s too)
```

Useful variables: `SKIP_BUILD=1` (reuse the image), `IGW_IMAGE`, `KLB_TAG`, `GIE_VERSION`,
`GWAPI_VERSION`, `VIP_POOL`.

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
| 5 | a pool with `endpointPickerRef` + `FailClose` is refused, with no Service | the picker is not silently ignored |
| 6 | switching it to `FailOpen` accepts it and the Service appears | the refusal is policy, not a parse failure |
| 7 | deleting the route deletes the Service | ownership is tracked and cleaned up |
| 8 | a request naming the model is answered by one of the pool's pods, and one naming another model is not | the rule carries traffic, and `model_name` selects rather than decorates |

Check 3 is the one that would have caught a payload regression on a real peer, and check 5 the one
that would catch a silent policy downgrade. 3a, 3b and 3c all exist because of failures this scenario
found the first time it ran.

## Notes

- **The VIP is the gateway's own address.** fullproxy binds a socket to it, and an address loxilb
  owns only as a `/32` rule device cannot be bound - `bind failed Cannot assign requested address`,
  after which the rule exists, reads back correctly, and nothing listens. `VIP_POOL` defaults
  accordingly.
- **The banner is the proof.** Model servers answer with their own pod name, so check 8 can say
  which endpoint served. A 200 alone would not separate "routed to a pod" from "answered by the
  proxy".
- The Inference Extension CRDs are installed at **v1.6.0**: `endpointPickerRef` is required in
  v1.5.0 and earlier, so the pools here - which omit it - would be rejected by the API server.
- `sel` and `mode` are asserted numerically on purpose. Names are a client-side convenience;
  the number is what goes on the wire and what loxilb acts on.
