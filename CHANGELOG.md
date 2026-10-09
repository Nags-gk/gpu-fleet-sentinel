# Changelog

## Unreleased

### Added
- Lease-based agent heartbeats: node condition is rewritten only on change or every `--condition-resync` (default 5m). Controller accepts either heartbeat. Helm: `agent.leaseHeartbeat` (default true), namespaced RBAC for the agent's Leases.
- Scale benchmark (`make scale`, `cmd/simfleet`, `hack/scale.sh`) with kwok nodes against a real apiserver; results in `docs/SCALE.md`.
- Agent metrics `sentinel_condition_patches_total` and `sentinel_lease_renew_errors_total`.

### Fixed
- XID 63 (row remap/page retirement *recorded*) is now a warning instead of a critical fault; XID 64 (failure) stays critical.
- A restarted agent seeds its debouncer from the existing `GPUHealthy` condition, so a faulty node no longer flaps to `True` (resetting the controller's grace timer) after an agent restart.

### Changed
- Release workflow now publishes multi-arch images with SBOM and provenance, signs them with keyless cosign, packages the Helm chart, and creates a GitHub release.
- Removed `docs/STUDY_GUIDE.md` from the repository.
