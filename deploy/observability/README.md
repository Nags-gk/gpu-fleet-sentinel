# Observability

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install mon prometheus-community/kube-prometheus-stack -n monitoring --create-namespace \
  -f deploy/observability/kube-prometheus-values.yaml

helm upgrade sentinel deploy/helm/gpu-fleet-sentinel -n gpu-sentinel --reuse-values \
  --set serviceMonitor.enabled=true --set prometheusRule.enabled=true --set grafanaDashboard.enabled=true

kubectl -n monitoring port-forward svc/mon-grafana 3000:80   # admin / sentinel
```

The **GPU Fleet Sentinel** dashboard (`grafana-dashboard.json`) shows managed vs
quarantined nodes, per-GPU temperature and health, findings by rule, remediation
decisions, evictions, time-to-quarantine percentiles and telemetry errors.

## Metrics

| Metric | Type | Source |
|---|---|---|
| `sentinel_gpu_temperature_celsius{gpu,uuid}` | gauge | agent |
| `sentinel_gpu_power_watts{gpu,uuid}` | gauge | agent |
| `sentinel_gpu_health_status{gpu,uuid}` | gauge (0/1/2) | agent |
| `sentinel_node_gpu_healthy` | gauge | agent |
| `sentinel_findings_total{rule,severity}` | counter | agent |
| `sentinel_sample_duration_seconds` | histogram | agent |
| `sentinel_sample_errors_total`, `sentinel_condition_patch_errors_total` | counter | agent |
| `sentinel_managed_nodes`, `sentinel_quarantined_nodes` | gauge | controller |
| `sentinel_remediation_decisions_total{action}` | counter | controller |
| `sentinel_evictions_total{result}` | counter | controller |
| `sentinel_time_to_quarantine_seconds` | histogram | controller |
