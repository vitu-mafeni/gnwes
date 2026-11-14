#!/bin/bash
# Repeated-cycle proof-of-concept for Use Case 1 (elastic GPU bursting):
# drives N suspend+resume cycles on the real exclusive-tier actor via
# kubectl-ate, and records each cycle's real attach/detach latency from
# VMGPUAttachment.status -- not simulated, read straight off the live
# cluster. Models an agent repeatedly bursting to a GPU for a tool call
# then releasing it.
#
# Confirmed live 2026-11-14: before the chclient.RemoveDevice/AddDevice
# vm.info-confirmation fixes, this reliably wedged after the very first
# cycle (cloud-hypervisor's own device manager reporting state that
# didn't match reality -- see internal/chclient/client.go's doc comments
# on both functions). After those fixes, 5/5 cycles succeeded, with
# attach consistently ~2ms -- that's the idempotent-recovery path
# confirming a device Agent Substrate's restore already baked back into
# the VM, not a from-scratch hotplug (which is 1-2s; see
# config/vmgpu/README.md).
#
# Run as: bash bench-attach-cycles.sh <cycles>
set -uo pipefail
CYCLES="${1:-5}"
KA=/home/ubuntu/go/bin/kubectl-ate
NS=ate-demo-counter
ACTOR=gnwes-actor-exclusive-1
ATTACHMENT=$(kubectl get vmgpuattachment -n "$NS" -o jsonpath='{.items[0].metadata.name}')
OUT=bench-results.csv
echo "cycle,op,phase,latency_ms,guest_bdf,timestamp" > "$OUT"

for i in $(seq 1 "$CYCLES"); do
  echo "== cycle $i/$CYCLES: suspend =="
  $KA suspend actor "$ACTOR" -a "$NS"
  sleep 8
  ST=$(kubectl get vmgpuattachment "$ATTACHMENT" -n "$NS" -o jsonpath='{.status.phase},{.status.attachLatencyMillis},{.status.guestBDF},{.status.lastTransitionAt}')
  echo "$i,detach,$ST" >> "$OUT"
  echo "  $ST"

  echo "== cycle $i/$CYCLES: resume =="
  $KA resume actor "$ACTOR" -a "$NS"
  sleep 8
  ST=$(kubectl get vmgpuattachment "$ATTACHMENT" -n "$NS" -o jsonpath='{.status.phase},{.status.attachLatencyMillis},{.status.guestBDF},{.status.lastTransitionAt}')
  echo "$i,attach,$ST" >> "$OUT"
  echo "  $ST"
done

echo
echo "== results =="
cat "$OUT"
