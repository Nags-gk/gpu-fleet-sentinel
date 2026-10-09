# Testing

| Layer | What it proves | Run |
|---|---|---|
| Unit tests | Each rule, policy branch and controller path, with the fake client | `make test` |
| **Property tests** | Safety invariants hold for thousands of random inputs (seeded, so failures reproduce) | `make test` |
| **Fuzz tests** | Untrusted input (exporter output, LLM responses) cannot crash or corrupt the pipeline | `make fuzz` |
| Integration (envtest) | Real kube-apiserver: patches, conflicts, Eviction API, PDBs, CRD admission (CEL) | `make test-integration` |
| End to end (kind) | Helm install, fault injection, reschedule, release | `make e2e` |
| Scale | 1,000 nodes, API load, budget under 150 simultaneous faults | `make scale` |

## Properties checked

`internal/remediation/property_test.go` (20,000 random policy/node/fleet cases)
- Never acts on a stale heartbeat or unknown health.
- A healthy node is never quarantined or deferred; an unhealthy one is never released.
- Quarantine needs the grace period elapsed **and** a free budget slot; deferral happens only when the budget is exhausted.
- Every `Wait`/`Defer` has a positive requeue no longer than any configured period, even when the node's clock is skewed.
- The budget never falls below `maxUnavailable` and never shrinks as the fleet grows.
- A simulated fleet (1-60 nodes, random health flips, clock jumps and serial reconciles, 1,000 seeds) never exceeds the budget, and always converges: recovered nodes are released and unhealthy ones fill the budget.

`internal/controller/property_test.go` runs the real reconciler over random fleets split across two `GPUNodePolicy` pools and the flag default: no policy exceeds its own budget, cordon and quarantine annotation always agree, and a quarantined node has no GPU pods left. A guard fails the test if the run never reached quarantines or budget contention, so it cannot pass vacuously.

`internal/health/property_test.go`: the debouncer flips state exactly when `failAfter` / `recoverAfter` consecutive reports are in, rule results do not depend on GPU ordering, and sub-threshold counter growth or a driver-reload reset never alerts.

## Fuzz targets

- `FuzzParseDCGM`: arbitrary exporter text; samples are sorted, unique, non-negative and finite.
- `FuzzEvaluate`: arbitrary readings; status equals the worst finding, output sorted and valid UTF-8.
- `FuzzChatCompletionsResponse`: a hostile LLM endpoint; accepted summaries are bounded valid UTF-8, and the template fallback always rescues a failure.
- `FuzzTruncate`: never splits a multi-byte character.

## Bugs these found

1. A `NaN`/`Inf` reading from the exporter parsed fine and compared false against every threshold, so a NaN temperature read as healthy. Negative or huge counters were converted to `uint64`, which Go leaves implementation-defined (amd64 and arm64 differ). Such scrapes now fail and report `Unknown`, which never triggers remediation.
2. If a node's clock ran ahead of the controller's, the elapsed time went negative and a node could wait nearly twice its configured recovery period ("healthy for -8m7s").
3. Three copies of `truncate` could split a multi-byte character, silently corrupting a condition message or annotation. They are now one `textutil.Truncate`, which fuzzing then found a further edge case in (limit smaller than the first rune).
4. While building the controller-level property test, the first version passed with **zero** quarantines: the fake client silently ignores status changes made through a plain `Update`. The non-vacuity guard exists because of that.
