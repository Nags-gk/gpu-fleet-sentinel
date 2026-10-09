# Scale benchmark: 1,000 GPU nodes

Reproduce with `make scale` (or `NODES=1000 FAULTS=150 MODES="legacy lease" hack/scale.sh`).
Raw reports: [`docs/scale-results/`](scale-results/).

## Setup

- A real **kube-apiserver 1.32 + etcd** (the envtest binaries) on one laptop (Apple Silicon, no Docker).
- **1,000 nodes** registered as [kwok](https://kwok.sigs.k8s.io/) fake nodes, each with 8 `nvidia.com/gpu` and one running GPU pod (kwok runs the pods and finishes evictions).
- **1,000 real agents** (the production `Agent` code path, simulated GPUs), 15 s interval, start times spread across the interval like a DaemonSet.
- The **real controller binary**: grace 10 s, recovery 20 s, disruption budget 10 % (= 100 nodes).
- `legacy` = patch every node's status every interval (the original behavior). `lease` = renew a small Lease every interval; rewrite the node condition only when it changes or every 5 min.
- Request counts are taken client-side by wrapping the agents' HTTP transport, over a 2-minute steady-state window after all agents converged.

## Results

| | legacy | lease |
|---|---|---|
| Node status writes / s (steady state) | **66.7** | **0** (≈3.3/s amortized from the 5-min resync) |
| Node GETs / s | 66.7 | 0 |
| Lease writes / s | 0 | 66.7 |
| Total agent requests / s | 133.3 | 66.7 |
| Controller reconcile mean | 3.9 ms | 3.2 ms |
| Controller RSS | 70 MiB | 71 MiB |

Fault scenario: 150 nodes hit XID 79 at once, then cleared.

| | legacy | lease |
|---|---|---|
| Quarantined / deferred by budget | 100 / 50 | 100 / 50 |
| Peak concurrently quarantined | 100 (budget 100) | 100 (budget 100) |
| Fault injected → condition `False` (p50 / p95) | 18.7 s / 23.7 s | 20.0 s / 24.0 s |
| Condition `False` → cordoned (p50 / p95 / max) | 10.8 / 10.9 / 11.8 s | 10.6 / 10.7 / 10.7 s |
| GPU pods evicted | 100 | 100 |
| Faults cleared → all nodes released | 49.8 s | 49.9 s |

## What this shows

- **Lease heartbeats cut Node object writes to ~zero** on a steady fleet. Every Node write is also a watch event delivered to every Node watcher in the cluster (scheduler, kubelets' informers, autoscaler, other controllers), so this matters more than the raw request count. Total agent requests halve because the per-tick Node GET is gone as well.
- **Behavior is unchanged:** identical quarantine counts, the disruption budget held at exactly 100 of 150 simultaneous failures, and the other 50 were deferred and never drained.
- **Controller overhead at 100 simultaneous quarantines is under 1.8 s** beyond the 10 s grace period (p95 0.7 s in lease mode). A single reconcile worker is not a bottleneck at this size: 3,034 reconciles averaged 3-4 ms.
- Detection latency (≈20 s) is the agent debounce: `failAfter=2` samples at a 15 s interval.

## Limits (read before quoting these numbers)

- One apiserver and one etcd on a laptop, no watch fan-out from real kubelets, no scheduler. Absolute capacity of a production control plane will differ; the *ratios* (writes avoided) are what transfer.
- Simulated GPUs: this validates the control loop and API load, not DCGM behavior.
- CPU columns in the raw JSON read ≈0 because `ps` cputime has coarse resolution over a 2-minute window on an idle laptop; they are not evidence of cost either way.
- Each reconcile lists the managed nodes from the informer cache to compute the budget, which is O(N) per reconcile. It is cheap at 1,000 nodes; at tens of thousands, shard the controller or keep a running counter.

## Re-run after adding `GPUNodePolicy`

Policy resolution lists every node (not just the label-selected ones) on each reconcile. Re-running the lease scenario at 1,000 nodes gave the same outcome (100 quarantined / 50 deferred, budget held, 100 pods evicted, all released in 50 s) with a reconcile mean of 3.9 ms (was 3.2 ms) and condition-`False`-to-cordon p95 of 11.1 s (was 10.7 s). The cost is O(nodes x policies) per reconcile, still negligible here.
