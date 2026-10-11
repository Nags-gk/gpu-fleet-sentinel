# Architecture and design decisions

## Components

**Agent** (`cmd/agent`, DaemonSet on GPU nodes)
1. `Source.Sample()`: scrape dcgm-exporter, or the simulator in `--source=sim`.
2. `Evaluator.Evaluate()`: rules produce `Finding{GPU, Rule, Severity, Message}`.
3. `Debouncer.Observe()`: hysteresis on the node-level status.
4. Publish the `GPUHealthy` condition (`True` / `False` / `Unknown`) plus metrics.

**Controller** (`cmd/controller`, Deployment, leader-elected)
1. Watches Nodes matching `nvidia.com/gpu.present=true`. Heartbeat-only updates are filtered out by a predicate.
2. Builds a `NodeView` + `FleetView` and calls `Policy.Decide()`.
3. Executes the action: requeue, defer (event), quarantine (cordon/taint/drain), or release.

The two halves only communicate through the Kubernetes API: one condition, one
taint, a few annotations. Either can be replaced independently. For example,
NVIDIA's Node Problem Detector could write the condition instead of the agent.

## Decisions and trade-offs

| Decision | Why | Trade-off |
|---|---|---|
| Node **condition** as the agent-to-controller interface | Standard Kubernetes signal; visible in `kubectl describe node`; Node Problem Detector uses the same pattern | Less structured than a CRD with per-GPU status; detail lives in the message and in `/debug/report` |
| `GPUNodePolicy` CRD for *configuration* only, with flags as the default policy | Per-pool grace, budget and dry-run without redeploying; status gives `kubectl` visibility; flag-only installs keep working | Policy precedence (first match by name) is one more thing to reason about; each node counts toward one policy so budgets cannot be double-spent |
| Repair escalation behind an actuator interface; default actuator only annotates | A reboot is the right fix for latched faults but needs host privilege; keeping "when" (pure, tested) apart from "how" lets a cluster use its own mechanism and keeps the default install unprivileged | Out of the box nothing reboots; operators wire an actuator or opt into the privileged pod |
| Record the repair attempt before acting | If the controller dies between the two, an attempt is lost instead of repeated, so reboots stay bounded | One fewer reboot than allowed in that rare case |
| Repair history survives release, expires with `historyWindow` | A node that recovers and fails again cannot get a fresh set of reboots every time | A node healthy for a day gets a fresh budget |
| Policy status written with `Update`, not a merge patch | A merge patch drops zero-valued counts, which the CRD requires; found only against a real apiserver | Conflicts retry on the next reconcile |
| Agent never cordons | Least privilege: a compromised or buggy agent on one node can't drain the fleet | One extra hop through the controller |
| Telemetry loss = `Unknown`, not `False` | An exporter crash is far more common than a GPU failure; draining on missing data turns a monitoring outage into a capacity outage | A truly dead node with a dead agent isn't caught by this system. The kubelet's `Ready` condition and cluster autoscaler cover that case |
| Debounce in the agent, grace period in the controller | Debounce filters sensor noise close to the source; the grace period is a fleet-operator policy knob | Two timers to tune |
| Pure `Decide()` function | Every branch is table-tested with no cluster; behavior is reviewable in one file | Controller code must translate cluster state into views |
| `MaxConcurrentReconciles: 1` | The budget check reads fleet state, then acts. Parallel workers could both see one free slot | Throughput: fine for a fleet in the tens of thousands of nodes because each reconcile is a few API calls |
| Strategic merge patch for the condition | Conditions merge by `type`, so the kubelet's conditions are never clobbered (a JSON merge patch would replace the whole list) | Only available for built-in types |
| Optimistic-lock merge patch for cordon/taint | Taints are a list; a stale read could delete another component's taint | Conflicts cause a retry |
| Eviction API, not delete | Honors PodDisruptionBudgets and graceful termination | A PDB can block a drain indefinitely, so it surfaces as an event and an alert |
| Disruption budget = max(absolute, percent) | Small fleets still get one slot; large fleets scale | A correlated failure (bad firmware rollout) is rate-limited by design, which is usually desired |
| LLM summary optional, template fallback | Remediation must never depend on an external model being up | Template hints are generic |

## Failure modes considered

| Failure | Behavior |
|---|---|
| dcgm-exporter down | Condition `Unknown` → no action; `GPUTelemetryUnavailable` alert |
| Agent pod crashes | Heartbeat goes stale → controller ignores the node's last state |
| Controller crashes mid-drain | Annotation marks the node quarantined; next leader re-runs the idempotent drain |
| Two controllers running | Leader election; only the leader reconciles |
| Correlated failure across many nodes | Budget caps quarantines; the rest get `RemediationDeferred` and an alert |
| PDB blocks eviction | Pod counted as blocked, node requeued in 30s, event + alert; other nodes are unaffected |
| Human cordons a node first | Recorded in an annotation; release leaves the cordon in place |
| Flapping GPU | Agent hysteresis plus the controller recovery period |

### Repair safety gates

| Gate | Enforced by |
|---|---|
| Only nodes already quarantined by us and still unhealthy | escalation runs inside the quarantine path |
| Never act on stale or unknown agent data | the policy `Decide` runs first and returns `None` |
| Wait `after` before the first step, `cooldown` between steps | `escalation.Spec.Decide` (property-tested) |
| At most `maxAttempts` reboots per `historyWindow`, then one replacement request | `escalation.Spec.Decide`; simulated for 2,000 random specs |
| At most `maxConcurrent` reboots in flight per policy | counted from node annotations |
| No reboot while any workload pod remains or an eviction is PDB-blocked | all-pods drain, then a pod count that includes terminating pods |
| Dry-run policies never repair | dry-run returns before the quarantine path |

## Not in scope (yet)

- Per-GPU isolation (marking one GPU unusable instead of the whole node). This needs device-plugin integration.
- Active diagnostics (`dcgmi diag`) before release.
- Topology-aware budgets (per rack or InfiniBand rail).
