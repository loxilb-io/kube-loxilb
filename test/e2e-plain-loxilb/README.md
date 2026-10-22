# e2e-plain-loxilb — kube-loxilb against upstream loxilb, two builds side by side

Runs the same checks against **upstream [loxilb](https://github.com/loxilb-io/loxilb)** for two
kube-loxilb builds - by default `upstream/main` and `HEAD` - on one testbed, and lays the outcomes
side by side. It answers whether a change breaks anything for a user who runs kube-loxilb with
plain loxilb, which is most users.

Where [test/wire-compat](../wire-compat/README.md) compares what goes on the wire against a fake
peer, this runs the real thing: real loxilb instances, real traffic, real restarts. Where
[test/e2e-inference-gwapi](../e2e-inference-gwapi/README.md) checks loxilb-inference-gateway,
this checks the paths every user goes through.

## Topology

```
   e2e-plain-net 13.13.13.0/24 (docker network; the host is 13.13.13.254)
     │
     ├─ llb1      13.13.13.1    loxilb; the host routes the Service pool 20.20.20.0/28 here
     ├─ llb2      13.13.13.2    loxilb; replaced and cut off by the checks, REST only
     └─ k3d node  13.13.13.128+ pods 10.42.0.0/16
```

kube-loxilb runs in the cluster from each build's own `manifest/ext-cluster/kube-loxilb.yaml`,
given both instances (`--loxiURL`), in fullnat mode (`--setLBMode=2`) so that replies come back
through loxilb from anywhere. Each build starts on fresh loxilb instances. Endpoints answer with
their pod name, so a request through the VIP says which endpoint served it.

## Requirements

`docker`, `k3d` (v5), `kubectl`, `jq`, `curl`, `git`, `python3` with `pyyaml`, and passwordless
`sudo` (`nsenter` for loxilb's routes and the cut-off in P5, and the host route to the pool).
The host needs to be free of anything else on 13.13.13.0/24 and 20.20.20.0/28.

## Run

```bash
./run.sh             # everything, detached; results in _work/results/<timestamp>/
```

`run.sh` survives the terminal that started it. It tears down any previous testbed, runs
`config.sh`, then `validation.sh` for the baseline and for the candidate, collects diagnostics,
and writes `summary.txt` - both builds, every check side by side, and `RESULT: PASS|FAIL` for the
candidate - then `done`. `_work/results/latest` points at the newest run.

By hand:

```bash
./config.sh                     # images of both builds, network, loxilb, k3d, workload
./validation.sh upstream/main   # the checks for one build
./validation.sh HEAD
./rmconfig.sh                   # teardown; images are kept
```

kube-loxilb is built from a clone at each commit, not from the working tree, and tagged by
commit. Useful variables: `KLB_BASE_REV`, `KLB_CAND_REV`, `LOXILB_IMAGE`, `K3S_IMAGE`,
`SKIP_BUILD=1` - see `env.sh` for the rest.

## What is checked

| # | Check | What it proves |
|---|---|---|
| P0 | kube-loxilb of that commit, from its own manifest, sees both instances | the build runs as shipped |
| P1 | a two-port Service and a pod-network Service get an address, a rule per port on both instances, and are served through the VIP by an endpoint pod | the basic path, end to end |
| P2 | scaling the Deployment 2 → 3 → 2 changes the pod-network rule's endpoints on both instances | endpoint changes reach every instance |
| P3 | restarting kube-loxilb leaves every rule and address as it was, and deletes nothing | a restart is not a reprogramming |
| P4 | llb2 replaced by a fresh container gets every rule back, without kube-loxilb restarting | an instance that comes back empty is filled again |
| P5 | with llb2's REST port dropped, a Service deleted and another created: the new one reaches llb1 within 30 s and is served; once llb2 answers again it gets the new one and loses the deleted one | an instance that stops answering - rather than refusing - holds nothing else up, and catches up after |
| P6 | 20 Services at once while llb2 is replaced twice: all reach both instances, and kube-loxilb neither restarts nor logs a crash | the cache and the instance coming back do not race |
| P7 | deleting every Service leaves no rule on either instance | nothing is left behind |

`compare.py` matches checks by section and description, with numbers masked, and prints each
outcome for both builds; `<>` marks a check whose outcome differs.

## Notes

- **P5 drops packets rather than stopping the instance.** A stopped container refuses
  connections, which fail at once; one that drops them makes every request wait for its timeout,
  which is what a partition or a hung host looks like.
- **kube-loxilb does not read rule status from plain loxilb**, so a rule deleted from an instance
  behind its back is not noticed there. This is not checked: neither build does it.
