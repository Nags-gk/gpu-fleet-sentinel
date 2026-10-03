# Runbook

## GPUNodeQuarantined

One or more GPU nodes were removed from scheduling.

1. `kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=NAME:.metadata.name,SUMMARY:.metadata.annotations.gpu-sentinel\.io/incident-summary`
2. `kubectl describe node <node>`: check the `GPUHealthy` condition message and the events.
3. Per-GPU detail: `kubectl -n gpu-sentinel port-forward pod/<agent-on-node> 9500` then `curl localhost:9500/debug/report`.
4. Act on the finding:
   - **XID 79 / missing GPU**: reboot the node. If it recurs, open a hardware ticket.
   - **XID 48 / double-bit ECC / XID 94-95**: reset the GPU, then run `dcgmi diag -r 3`.
   - **Row-remap failure (XID 64)**: RMA the GPU.
   - **Overheat**: check cooling and inlet temperature.
5. The node is released automatically after it reports healthy for the recovery period.

## GPURemediationBudgetExhausted

More nodes are unhealthy than the budget allows. This usually means a correlated
cause: a bad driver or firmware rollout, a cooling failure in one row, or a
switch failure.

1. `kubectl get events -A --field-selector reason=RemediationDeferred`
2. Look for a common factor across the affected nodes (labels, rack, driver version).
3. Fix the cause. Only raise `controller.maxUnavailablePercent` if the capacity loss is acceptable.

## GPUEvictionsBlockedByPDB

A quarantined node can't finish draining because a PodDisruptionBudget allows no disruptions.

1. `kubectl get events -A --field-selector reason=EvictionBlocked`
2. `kubectl get pdb -A` and find the owner of the blocking workload.
3. Coordinate with the owner: scale the workload up, or evict it manually once safe.

## GPUTelemetryUnavailable

The agent cannot read dcgm-exporter. The node's condition is `Unknown`, so it will
**not** be remediated automatically while this lasts.

1. `kubectl -n gpu-operator get pods -o wide | grep dcgm` on the affected node.
2. Check the agent's `--dcgm-url` and that the exporter is reachable from the node IP.
