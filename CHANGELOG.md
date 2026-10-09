# Changelog

## Unreleased

### Added
- Property tests (policy invariants, fleet simulation against the disruption budget, debouncer, evaluator, reconciler over random multi-policy fleets) and fuzz targets (`make fuzz`, run in CI). See `docs/TESTING.md`.
- `GPUNodePolicy` CRD (`gpu-sentinel.io/v1alpha1`): per-pool grace/recovery/stale periods, disruption budget, drain scope and dry-run, each with its own budget, plus status counts and conditions. CEL admission rejects empty selectors and invalid durations. Flags remain the default policy; installs without the CRD are unchanged. Helm: `crds/`, `policies:` value, RBAC. `make generate` rebuilds deepcopy and the CRD; CI fails if they drift.
- Lease-based agent heartbeats: node condition is rewritten only on change or every `--condition-resync` (default 5m). Controller accepts either heartbeat. Helm: `agent.leaseHeartbeat` (default true), namespaced RBAC for the agent's Leases.
- Scale benchmark (`make scale`, `cmd/simfleet`, `hack/scale.sh`) with kwok nodes against a real apiserver; results in `docs/SCALE.md`.
- Agent metrics `sentinel_condition_patches_total` and `sentinel_lease_renew_errors_total`.

### Fixed
- Non-finite or out-of-range exporter readings (NaN, Inf, negative or huge counters, XID outside 0-65535) now fail the scrape and report `Unknown` instead of reading as healthy.
- Clock skew placing a condition transition in the future no longer stretches the grace/recovery wait beyond its configured period.
- Truncating condition messages and annotations can no longer split a multi-byte character (single shared `textutil.Truncate`).
- XID 63 (row remap/page retirement *recorded*) is now a warning instead of a critical fault; XID 64 (failure) stays critical.
- A restarted agent seeds its debouncer from the existing `GPUHealthy` condition, so a faulty node no longer flaps to `True` (resetting the controller's grace timer) after an agent restart.

### Changed
- Release workflow now publishes multi-arch images with SBOM and provenance, signs them with keyless cosign, packages the Helm chart, and creates a GitHub release.
- Removed `docs/STUDY_GUIDE.md` from the repository.
