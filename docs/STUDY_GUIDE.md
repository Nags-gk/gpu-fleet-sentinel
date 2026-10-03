# Study guide: owning this codebase

Interviewers for infrastructure roles will ask *why* you built something a
certain way. Work through this guide until you can answer every question
without notes, and do at least two of the exercises yourself.

## Reading order (about 3 hours)

1. `internal/health/types.go`, then `rules.go`: what a finding is and how rules fire.
2. `internal/health/health_test.go`: the table-driven tests show every rule's behavior.
3. `internal/remediation/policy.go` + its test. The whole safety story lives in one function.
4. `internal/agent/agent.go`: the `Tick` loop and `publish` (strategic merge patch).
5. `internal/controller/node_controller.go`: `Reconcile` → `quarantine` → `drain` → `release`.
6. `internal/controller/evictor.go` + `evictor_test.go`: the 429 bug and its fix.
7. `test/integration/integration_test.go`: how envtest runs a real apiserver.
8. `deploy/helm/.../templates`: RBAC first, then the DaemonSet and Deployment.

## Concepts to be able to explain

- **Reconciliation loop / level-triggered control**: the controller compares desired and actual state each time, instead of reacting to individual events. Why `RequeueAfter` instead of sleeping.
- **Informers, caches and field indexes**: why `List` with `MatchingFields{spec.nodeName}` needs `IndexField`.
- **Leader election**: what a Lease is, and what happens during failover.
- **Cordon vs taint vs drain**: `spec.unschedulable` vs `NoSchedule` vs `NoExecute`, and why both a cordon and a taint are used.
- **Eviction API vs delete**: PodDisruptionBudgets, and why Pending pods skip them.
- **Patch types**: JSON merge, strategic merge, JSON patch, and optimistic locking with `resourceVersion`.
- **DCGM and XIDs**: what dcgm-exporter is; XID 79 vs 13; SBE vs DBE ECC; row remapping.
- **Prometheus**: counter vs gauge vs histogram; why `rate()` on counters; `histogram_quantile`.

## Likely interview questions

1. *Why doesn't the agent cordon the node itself?* Least privilege and blast radius: a buggy agent could drain the fleet. Splitting detection from action also lets the controller enforce a fleet-wide budget that no single node can see.
2. *What if dcgm-exporter crashes on 500 nodes?* Conditions go `Unknown`; the policy takes no action; an alert fires. Draining on missing data would turn a monitoring outage into a capacity outage.
3. *How do you stop two nodes from both taking the last budget slot?* One reconcile worker. Alternatives: a Lease or ConfigMap acting as a counter with optimistic locking, or a per-rack budget. Trade-off: throughput.
4. *Walk me through the 429 bug.* Integration test against a real apiserver; a PDB-protected pod; client-go honors `Retry-After` up to 10 times; one worker means a fleet-wide stall of 1m40s; fixed with `MaxRetries(0)`; regression test asserts one request in under 2s; verified the test fails without the fix.
5. *Why a strategic merge patch for the condition?* Conditions merge by `type`, so the kubelet's conditions survive. A JSON merge patch replaces the entire list and races with the kubelet.
6. *How would you scale this to 50,000 nodes?* Shard the controller by label or region, a per-rack/topology budget, rate-limited workqueues, server-side apply, and watch only the fields you need (metadata-only informers for pods).
7. *What does it NOT handle?* Per-GPU isolation, active diagnostics before release, a node with both a dead agent and a dead kubelet.
8. *How did you test it?* Three layers: pure unit tests (policy, rules), fake-client controller tests (quarantine/drain/release, budget, PDB), envtest against a real apiserver, and kind e2e with Helm.
9. *Why `xid13` is only a warning?* XID 13 is usually an application fault (illegal memory access in a kernel), not hardware; draining a node for a user bug wastes capacity.
10. *How would you add AMD GPUs?* Implement another `Source` (AMD's device-metrics exporter) mapping to the same `GPUSample`. Rules and controller are vendor-neutral.

## Exercises (do these yourself; they make the code yours)

1. **Add a rule**: power-throttling or a sustained-low-utilization warning, with a table test.
2. **Per-rack budget**: add `topology.kubernetes.io/zone` grouping to `FleetView` and `Budget`.
3. **Active diagnostics**: before release, require an annotation `gpu-sentinel.io/diag-passed=true`.
4. **Run `make e2e`** on kind and paste the timing table into the README.
5. **Run on a real GPU** for an hour (docs/AKS.md) and record real XID-free baseline metrics.
6. **Record a 60-second demo GIF** (`make demo`) for the README.
