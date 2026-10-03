#!/usr/bin/env bash
# End-to-end test on a kind cluster (see deploy/kind). Exercises the real
# Kubernetes API: fault → GPUHealthy=False → cordon/taint → GPU pod evicted and
# rescheduled → fault cleared → node released, then the disruption budget.
set -euo pipefail

NS=${NAMESPACE:-gpu-sentinel}
RELEASE=${RELEASE:-sentinel}
PF_PIDS=()

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
pass() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; dump; exit 1; }

cleanup() { for p in "${PF_PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

dump() {
  echo "---- diagnostics ----"
  kubectl get nodes -L nvidia.com/gpu.present -o wide || true
  kubectl -n "$NS" get pods -o wide || true
  kubectl -n "$NS" logs deploy/"$RELEASE"-controller --tail=80 || true
  kubectl get events -A --field-selector involvedObject.kind=Node --sort-by=.lastTimestamp | tail -30 || true
}

# wait_for <timeout-seconds> <description> <command...>
wait_for() {
  local timeout=$1 desc=$2; shift 2
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    (( SECONDS - start >= timeout )) && fail "timed out after ${timeout}s waiting for: $desc"
    sleep 1
  done
  pass "$desc ($((SECONDS - start))s)"
}

cond()    { kubectl get node "$1" -o jsonpath='{.status.conditions[?(@.type=="GPUHealthy")].status}'; }
is_cond() { [[ "$(cond "$1")" == "$2" ]]; }
tainted() { kubectl get node "$1" -o jsonpath='{.spec.taints[*].key}' | grep -q 'gpu-sentinel.io/unhealthy'; }
untainted() { ! tainted "$1" && [[ "$(kubectl get node "$1" -o jsonpath='{.spec.unschedulable}')" != "true" ]]; }

agent_port() { # agent_port <node> <local-port>: port-forward to that node's agent
  local pod
  pod=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=agent \
        --field-selector spec.nodeName="$1" -o jsonpath='{.items[0].metadata.name}')
  kubectl -n "$NS" port-forward "pod/$pod" "$2:9500" >/dev/null 2>&1 &
  PF_PIDS+=($!)
  wait_for 15 "port-forward to agent on $1" curl -sf "localhost:$2/healthz"
}

inject() { # inject <local-port> <gpu> <fault>
  curl -sf -X POST "localhost:$1/debug/inject" -H 'Content-Type: application/json' \
       -d "{\"gpu\":$2,\"fault\":\"$3\"}" >/dev/null || fail "inject $3 failed"
}

log "Waiting for sentinel to be ready"
kubectl -n "$NS" rollout status ds/"$RELEASE"-agent --timeout=120s
kubectl -n "$NS" rollout status deploy/"$RELEASE"-controller --timeout=120s

mapfile -t GPU_NODES < <(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
(( ${#GPU_NODES[@]} >= 3 )) || fail "need at least 3 GPU nodes, found ${#GPU_NODES[@]}"
pass "found ${#GPU_NODES[@]} simulated GPU nodes"

log "Advertising fake nvidia.com/gpu capacity so GPU pods can schedule"
for n in "${GPU_NODES[@]}"; do
  kubectl patch node "$n" --subresource=status --type=json \
    -p '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"}]' >/dev/null
done
for n in "${GPU_NODES[@]}"; do wait_for 60 "GPUHealthy=True on $n" is_cond "$n" True; done

log "Starting a GPU training workload"
kubectl create namespace ml --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n ml apply -f - >/dev/null <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: trainer}
spec:
  replicas: 1
  selector: {matchLabels: {app: trainer}}
  template:
    metadata: {labels: {app: trainer}}
    spec:
      nodeSelector: {nvidia.com/gpu.present: "true"}
      terminationGracePeriodSeconds: 1
      containers:
        - name: train
          image: registry.k8s.io/pause:3.10
          resources: {limits: {nvidia.com/gpu: 1}}
EOF
kubectl -n ml rollout status deploy/trainer --timeout=120s >/dev/null
TARGET=$(kubectl -n ml get pod -l app=trainer -o jsonpath='{.items[0].spec.nodeName}')
OLD_POD=$(kubectl -n ml get pod -l app=trainer -o jsonpath='{.items[0].metadata.name}')
pass "trainer pod $OLD_POD running on $TARGET"

log "Scenario 1: GPU falls off the bus (XID 79) on $TARGET"
agent_port "$TARGET" 19500
T0=$SECONDS
inject 19500 3 xid79
wait_for 30 "GPUHealthy=False on $TARGET" is_cond "$TARGET" False
wait_for 60 "$TARGET cordoned and tainted" tainted "$TARGET"
DETECT_TO_QUARANTINE=$((SECONDS - T0))

trainer_moved() {
  local node phase
  node=$(kubectl -n ml get pod -l app=trainer -o jsonpath='{.items[0].spec.nodeName}')
  phase=$(kubectl -n ml get pod -l app=trainer -o jsonpath='{.items[0].status.phase}')
  [[ -n "$node" && "$node" != "$TARGET" && "$phase" == "Running" ]] && ! kubectl -n ml get pod "$OLD_POD" >/dev/null 2>&1
}
wait_for 60 "GPU workload evicted and rescheduled onto a healthy node" trainer_moved
RESCHEDULED=$((SECONDS - T0))

SUMMARY=$(kubectl get node "$TARGET" -o jsonpath='{.metadata.annotations.gpu-sentinel\.io/incident-summary}')
[[ "$SUMMARY" == *"PCIe"* ]] || fail "incident summary missing or wrong: $SUMMARY"
pass "incident summary: $SUMMARY"

log "Scenario 1b: fault cleared, node returns to service after recovery period"
T1=$SECONDS
inject 19500 3 none
wait_for 30 "GPUHealthy=True on $TARGET" is_cond "$TARGET" True
wait_for 90 "$TARGET released (taint removed, uncordoned)" untainted "$TARGET"
RECOVERY=$((SECONDS - T1))

log "Scenario 2: two nodes fail at once; disruption budget allows only one quarantine"
mapfile -t OTHERS < <(printf '%s\n' "${GPU_NODES[@]}" | grep -vx "$TARGET" | head -2)
agent_port "${OTHERS[0]}" 19501
agent_port "${OTHERS[1]}" 19502
inject 19501 0 ecc-dbe
inject 19502 5 overheat
wait_for 30 "both nodes report GPUHealthy=False" bash -c "
  [[ \$(kubectl get node ${OTHERS[0]} -o jsonpath='{.status.conditions[?(@.type==\"GPUHealthy\")].status}') == False ]] &&
  [[ \$(kubectl get node ${OTHERS[1]} -o jsonpath='{.status.conditions[?(@.type==\"GPUHealthy\")].status}') == False ]]"
count_quarantined() { kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.gpu-sentinel\.io/quarantined-at}{"\n"}{end}' | grep -c . || true; }
wait_for 60 "exactly one node quarantined" bash -c "[[ \$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.gpu-sentinel\.io/quarantined-at}{\"\n\"}{end}' | grep -c .) == 1 ]]"
wait_for 60 "RemediationDeferred event recorded for the second node" bash -c \
  "kubectl get events -A --field-selector reason=RemediationDeferred -o name | grep -q ."
sleep 5
[[ $(count_quarantined) == 1 ]] || fail "budget violated: $(count_quarantined) nodes quarantined"
pass "budget held: still exactly one node quarantined"

log "Cleaning up faults"
inject 19501 0 none
inject 19502 5 none
for n in "${OTHERS[@]}"; do wait_for 120 "$n released" untainted "$n"; done

cat <<EOF

========================================================
 E2E PASSED
 fault injected → node quarantined : ${DETECT_TO_QUARANTINE}s  (grace period 10s)
 fault injected → workload moved   : ${RESCHEDULED}s
 fault cleared  → node released    : ${RECOVERY}s  (recovery period 20s)
 disruption budget                 : held under concurrent failures
========================================================
EOF
