#!/usr/bin/env bash
# Scale benchmark: N kwok nodes, one real agent per node, the real controller,
# and a real kube-apiserver + etcd (envtest binaries; no Docker needed).
#
#   NODES=1000 FAULTS=150 MODES="legacy lease" hack/scale.sh
#
# Requires: go, kwok (brew install kwok), and the envtest assets
# (KUBEBUILDER_ASSETS, see `make envtest`). Results land in scale-results/.
set -euo pipefail
cd "$(dirname "$0")/.."

NODES=${NODES:-1000}
STEADY=${STEADY:-120s}
FAULTS=${FAULTS:-150}
MODES=${MODES:-"legacy lease"}
INTERVAL=${INTERVAL:-15s}
OUT=${OUT:-scale-results}
: "${KUBEBUILDER_ASSETS:?set KUBEBUILDER_ASSETS (go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.32.0 -p path)}"
command -v kwok >/dev/null || { echo "kwok not found (brew install kwok)"; exit 1; }

mkdir -p bin "$OUT"; OUT=$(cd "$OUT" && pwd)
go build -o bin/ ./cmd/controller ./cmd/simfleet ./hack/scale/apiserver

pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

run_mode() {
  local mode=$1 kc="$OUT/$1.kubeconfig" lease_ns=default
  [[ $mode == legacy ]] && lease_ns=""
  echo "=== mode=$mode nodes=$NODES faults=$FAULTS ==="

  rm -f "$kc"
  bin/apiserver --write-kubeconfig "$kc" >"$OUT/$mode.apiserver.log" 2>&1 & pids+=($!)
  for _ in $(seq 1 120); do [[ -s $kc ]] && break; sleep 0.5; done
  [[ -s $kc ]] || { echo "apiserver did not start"; cat "$OUT/$mode.apiserver.log"; exit 1; }

  kwok --kubeconfig "$kc" --manage-all-nodes=false \
       --manage-nodes-with-annotation-selector=kwok.x-k8s.io/node=fake \
       --server-address=127.0.0.1:0 >"$OUT/$mode.kwok.log" 2>&1 & local kwok_pid=$!; pids+=($kwok_pid)

  KUBECONFIG="$kc" bin/controller --leader-elect=false --metrics-bind-address=:18080 \
       --health-probe-bind-address=:18081 --grace-period=10s --recovery-period=20s \
       --stale-after=2m --max-unavailable=1 --max-unavailable-percent=10 \
       --lease-namespace="$lease_ns" >"$OUT/$mode.controller.log" 2>&1 & local ctl_pid=$!; pids+=($ctl_pid)

  local api_pid etcd_pid
  api_pid=$(pgrep -f 'k8s/.*/kube-apiserver' | head -1)
  etcd_pid=$(pgrep -f 'k8s/.*/etcd' | head -1)
  local budget=$(( NODES * 10 / 100 )); (( budget < 1 )) && budget=1

  bin/simfleet --cluster "$kc" --nodes "$NODES" --mode "$mode" --interval "$INTERVAL" \
       --steady "$STEADY" --fault-nodes "$FAULTS" --budget "$budget" \
       --controller-metrics http://localhost:18080/metrics \
       --watch-pids "apiserver=$api_pid,etcd=$etcd_pid,controller=$ctl_pid,kwok=$kwok_pid" \
       --out "$OUT/$mode.json"

  cleanup; pids=()
}

for m in $MODES; do run_mode "$m"; done
echo "results in $OUT/"
