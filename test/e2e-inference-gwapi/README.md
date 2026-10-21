# e2e-inference-gwapi — InferencePool end to end

Proves the [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/)
path all the way down: an `InferencePool` referenced from an HTTPRoute becomes a LoadBalancer
Service, which becomes an **inference rule on a real loxilb-inference-gateway**, which then serves
a request.

Where [test/wire-compat](../wire-compat/README.md) asks *"did the payload change?"* against a fake
peer, this asks *"does the whole thing work?"* against the real one.

## Topology

```
   e2e-net 12.12.12.0/24 (docker network; the host is 12.12.12.254)
     │
     ├─ igw1      12.12.12.1    loxilb-inference-gateway under test
     │                          REST :11111, VIP 12.12.12.1:8080 - all traffic checks
     ├─ igw2      12.12.12.2    a second peer, replaced, paused and downgraded by the
     │                          credential checks; only its REST state is checked
     ├─ e2e-oidc  12.12.12.10   stand-in OIDC provider: discovery document and JWKS
     ├─ e2e-pg    12.12.12.11   PostgreSQL, the gateways' API-key store
     └─ k3d node  12.12.12.128+ pods 10.42.0.0/16, services 10.43.0.0/16
```

The gateways run as containers beside a one-node k3d cluster on one docker network, which is how
the loxilb cicd scenarios model an external load balancer. kube-loxilb runs in the cluster and is
given both gateways (`--loxiURL`), so every rule goes to both. Endpoints are **pod** addresses -
`usepodnetwork` is imposed by the controller - so each gateway is given a route into the cluster
network through the node. Without it the rule is programmed and every endpoint is unreachable.

igw2 is given the same rules as igw1. A gateway refuses a fullproxy rule whose address it cannot
bind - unless it is a cluster standby, which kube-loxilb without `--setRoles` does not make it - so
igw2 holds the VIP on `lo` with `arp_ignore=1`: it can bind the listener, and the VIP's traffic
still reaches only igw1. That is each gateway holding the VIP as with BGP anycast, without the
announcement. igw2 serves nothing; it is there to be the peer that goes away, comes back empty, or
cannot hold a rule.

Both gateways use the PostgreSQL API-key store (`--aikey-db-*`), bootstrapped with the gateway's own
`scripts/aigw-db-bootstrap.sql` as its `cicd/ai-jwtauth` scenario does. A gateway without a store
verifies a token and then answers `503 policy_store_unavailable`: its QoS stage fails closed on an
identity it has no store to look up, so JWT alone is not a working configuration.

Model servers are `socat` answering with their own pod name. The banner is the delivery proof:
it identifies which endpoint actually served the request, which metrics alone cannot.

## Requirements

`docker`, `k3d` (v5), `kubectl`, `jq`, `curl`, `git`, `python3` with `pyyaml`, `pyjwt` and
`cryptography` (the token minting), and passwordless `sudo` (`nsenter` for the gateways' routes and
sockets - the gateway image has no `ip`). The host needs to be free of other loxilb cicd runs - the
scenarios use fixed container names - and of anything else on 12.12.12.0/24.

The gateway image is built from a loxilb-inference-gateway checkout by `build-igw.sh`, which
leaves the checkout's working tree alone; `IGW_IMAGE` names one instead. The credential checks
also need a gateway build **without** the credential features for igw2 to be downgraded to
(`IGW_OLD_IMAGE`, a local image); without it check A7 is skipped. `build-igw.sh` also copies the
API-key store's bootstrap script out of the same commit; the store runs from `postgres:18.6`.

The Endpoint Picker checks (5-6d, including 5c) also need `go`, a loxilb-inference-gateway checkout in `EPP_SRC`
(its `cmd/epp-fake` becomes the stand-in picker), and an `IGW_IMAGE` whose `GET /version` lists
`"epp"` in `features`. Without either, those checks report `[SKIPPED]` and the rest still run.

## Run

```bash
IGW_REPO=~/go/src/loxilb-io/loxilb-inference-gateway ./build-igw.sh   # gateway image, once per commit
./run.sh             # everything below, detached; results in _work/results/<timestamp>/
```

`run.sh` survives the terminal that started it. It tears down any previous testbed, runs
`config.sh`, `validation.sh` and `validation-auth.sh`, collects diagnostics, and writes
`summary.txt` - what was tested, each stage's exit code, every `[FAIL]` and `[NOTE]` line, and
`RESULT: PASS|FAIL` - then `done`. `_work/results/latest` points at the newest run. The testbed is
left up afterwards; `KUBECONFIG=_work/kubeconfig` reaches it.

By hand:

```bash
./config.sh            # ~5 min: kube-loxilb images, network, gateways, OIDC stub, k3d, CRDs, fixtures
EPP_SRC=~/src/loxilb-inference-gateway IGW_IMAGE=<gateway with "epp"> ./config.sh   # with the picker
./validation.sh        # checks 1-9
./validation-auth.sh   # checks A0-A11, after validation.sh
./rmconfig.sh          # teardown; images are kept
```

kube-loxilb is built from a clone at `KLB_REV` (default `HEAD`), not from the working tree, and
tagged by commit, so a rerun at the same commit reuses the image. `KLB_OLD_REV` (default
`474474c`, before rule ids) is built too, for the upgrade check.

Useful variables: `IGW_IMAGE`, `IGW_OLD_IMAGE`, `KLB_REV`, `KLB_OLD_REV`, `SKIP_BUILD=1` (fail
rather than build), `K3S_IMAGE`, `GIE_VERSION`, `GWAPI_VERSION`, `VIP_POOL`, `EPP_SRC`,
`EPP_IMAGE` - see `env.sh` for the rest.

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
| 9 | deleting the pool's route leaves no pool rule on the VIP:port (the Gateway's own ingress Service takes the listener back, and its rule is left out) | `model_name` is part of loxilb's rule key and has to be sent on the delete — get it wrong and the rule outlives its Service, still bound and still answering |

### Credential checks (`validation-auth.sh`)

Tokens come from `oidc-stub.py`: RS256, `iss` the stub's realm, `aud` `kube-e2e`, `tenant_id`,
and `realm_access.roles` carrying `model:<name>` - the claims a Keycloak realm gives and the
gateway's defaults read.

| # | Check | What it proves |
|---|---|---|
| A0 | both peers list `api_key_auth` and `jwt_auth_profile` in `/version` | the testbed is what the rest assumes |
| A1 | rules made by a kube-loxilb from before rule ids get, once it is upgraded, a UUIDv5 id per port, the same on both peers and across a restart - with no rule deleted or re-added on the way | the id is bound in place by a replace; each port of a Service is its own rule |
| A2 | a pool naming a profile with no `JWTAuthProfile`: `ProfileNotFound` on the pool and Service, no rule on either peer | a rule is not sent ahead of its profile |
| A3 | the `JWTAuthProfile` is `Programmed`, holds its finalizer and is on both peers; the rule then follows with `api_key_auth`/`jwt_auth_profile` and the same id on both | the profile reaches every peer before the rule naming it - the gateway refuses the rule otherwise |
| A4 | through the VIP: a token with the model's role is served by a pool pod; no token 401, another model's role 403, another key's signature 401, another audience 401 | the policy reached the data plane, and against the stub's keys |
| A5 | the rule deleted from igw2 over REST is put back, with its id, on the next reconcile, and `RuleMissing` is reported | a rule lost behind kube-loxilb's back is found and restored |
| A6 | igw2 replaced by a fresh container - no snapshot - gets the profile and then the rule, without kube-loxilb restarting | a returning peer gets what its rules need first |
| A7 | igw2 downgraded to a build without the features: `AuthNotSupported` on the pool and Service, the rule is not sent there, plain rules still are, igw1 is unaffected; upgraded again, it gets the profile and the rule | a peer that would serve the rule unauthenticated never gets it; the features are read again when the peer comes back |
| A8 | with igw2's REST port dropped, the pool's credential annotations are removed: igw1 loses the policy at once; once igw2 answers again, its rule - sent without the policy, which the gateway keeps - is found on read-back and re-added without it | a replace that omits `api_key_auth` keeps the stored one, and the read-back is what catches it on a peer that missed the change |
| A9 | a second pool on the same listener declaring no policy is held with `ListenerAuthConflict`; once the older pool goes, it takes the listener and is served without a token | the oldest pool's policy holds a listener, and the one that yielded is released |
| A10 | deleting the `JWTAuthProfile` while a rule names it: `ProfileInUse`, kept on both peers and still served; once nothing names it, it goes from both peers and from the cluster | the finalizer keeps a profile in use, and lets it go after |
| A11 | no pool rule left on the VIP:port on either peer | everything the checks created is removed |

A7 also reports, as a `[NOTE]`, whether the old build keeps the rule ids it is sent: one that does
not has each rule re-added once after it is installed, when it is read back by id.

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
- **kube-loxilb looks at the rules on a reconcile.** A5 touches the Service (an annotation outside
  `loxilb.io/`) to get one; without an event, a rule deleted from a peer stays missing until the
  next one.
- **A profile is deleted up to two minutes after its last rule.** While a rule names it the
  gateway refuses the delete, and kube-loxilb retries with backoff up to 120 s apart.
- **A8 drops igw2's REST port rather than pausing it.** A paused container's kernel still accepts
  connections, and requests sent meanwhile run the moment it resumes - the peer misses nothing.
  A8 also notes, as `[NOTE]`, how long a sync of the pool's Service took while igw2 did not answer.
- **Older gateway images start a shell.** `env.sh` always names the loxilb binary as the
  entrypoint, so a build of either kind can stand in for igw2.
- The stand-in picker serves a self-signed certificate, as the llm-d EPP does, so the gateway's
  default transport (TLS without verification) is what is exercised.
- The Inference Extension CRDs are installed at **v1.6.0**: `endpointPickerRef` is required in
  v1.5.0 and earlier, so the pools here - which omit it - would be rejected by the API server.
- `sel` and `mode` are asserted numerically on purpose. Names are a client-side convenience;
  the number is what goes on the wire and what loxilb acts on.
