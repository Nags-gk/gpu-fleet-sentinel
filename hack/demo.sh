#!/usr/bin/env bash
# Interactive demo for screen recording: watch nodes in one terminal
# (`watch -n1 kubectl get nodes -L nvidia.com/gpu.present`) and run this in another.
set -euo pipefail
NS=${NAMESPACE:-gpu-sentinel}
NODE=${1:-$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')}
FAULT=${2:-xid79}

POD=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=agent --field-selector spec.nodeName="$NODE" \
      -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" port-forward "pod/$POD" 19600:9500 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF' EXIT; sleep 2

echo "Injecting $FAULT on GPU 3 of $NODE"
curl -s -X POST localhost:19600/debug/inject -d "{\"gpu\":3,\"fault\":\"$FAULT\"}"; echo
echo "Agent report:"; curl -s localhost:19600/debug/report; echo
read -rp "Press enter once the node shows SchedulingDisabled to clear the fault... "
curl -s -X POST localhost:19600/debug/inject -d '{"gpu":3,"fault":"none"}'; echo
echo "Node events:"; kubectl get events --field-selector involvedObject.name="$NODE" --sort-by=.lastTimestamp | tail -8
echo "Incident summary:"; kubectl get node "$NODE" -o jsonpath='{.metadata.annotations.gpu-sentinel\.io/incident-summary}'; echo
