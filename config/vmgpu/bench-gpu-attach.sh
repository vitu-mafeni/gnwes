#!/bin/bash
# Benchmarks real GPU hot-attach/detach latency on the Exclusive tier,
# against real hardware -- not a simulation. Run on the cluster's
# control-plane node (wherever `kubectl` and `kubectl-ate` already work),
# against a WorkerPool using the microvm-nvidia-gpu SandboxConfig (see
# README.md), with the gnwes-nvidia-smi actor template already created
# (see nvidia-smi-test/README.md).
#
# What this measures, per trial:
#   - resume_cmd_wall_ms   -- wall-clock time `kubectl-ate resume actor`
#     blocks for (actor creation through ACTOR_STATE_RUNNING): the real
#     microVM cold-boot cost, nothing GPU-specific.
#   - attach_poll_wall_ms  -- wall-clock time from the moment resume
#     returns until the real VMGPUAttachment reaches phase=Attached, as
#     observed by this script's own polling (includes
#     cmd/substrate-gpu-bridge's own poll-interval latency, not just the
#     attach operation itself).
#   - reconciler_attachMs / reconciler_detachMs -- the reconciler's own
#     measured latency (AttachLatencyMillis in VMGPUAttachmentReconciler),
#     i.e. just the vm.add-device / vm.remove-device call plus its own
#     pool bookkeeping -- this is the number comparable to a warm-pool
#     "lease attach" latency.
#   - the actor's own "Actor boot phases" log line (agent_dial, containers,
#     since_boot, ...), pulled from the worker pod's log, which breaks the
#     microVM cold-boot cost down further.
#
# Usage: bash bench-gpu-attach.sh [namespace] [worker-pool-label] [trials] [template]
set -u
NS="${1:-ate-demo-counter}"
POOL_SELECTOR="${2:-ate.dev/worker-pool=gnwes-counter-exclusive}"
TRIALS="${3:-5}"
TEMPLATE="${4:-gnwes-nvidia-smi}"
PREFIX="gnwes-bench-$$"

POD=$(kubectl get pods -n "$NS" -l "$POOL_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
echo "worker pod: $POD"
echo "trials: $TRIALS, namespace: $NS, template: $TEMPLATE"
echo

wait_phase() {
  local want="$1" deadline_s="$2"
  local start
  start=$(date +%s%N)
  while true; do
    phase=$(kubectl get vmgpuattachment -n "$NS" "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$phase" = "$want" ]; then
      echo $(( ($(date +%s%N) - start) / 1000000 ))
      return 0
    fi
    now=$(date +%s%N)
    if [ $(( (now - start) / 1000000000 )) -gt "$deadline_s" ]; then
      echo "-1"
      return 1
    fi
    sleep 0.02
  done
}

for i in $(seq 1 "$TRIALS"); do
  ACTOR="${PREFIX}-${i}"
  echo "=== trial $i: $ACTOR ==="

  kubectl-ate create actor "$ACTOR" -a "$NS" --template "$TEMPLATE" >/dev/null 2>&1

  t0=$(date +%s%N)
  kubectl-ate resume actor "$ACTOR" -a "$NS" >/dev/null 2>&1
  resume_cmd_wall_ms=$(( ($(date +%s%N) - t0) / 1000000 ))

  attach_poll_wall_ms=$(wait_phase "Attached" 30)
  reconciler_attachMs=$(kubectl get vmgpuattachment -n "$NS" "$POD" -o jsonpath='{.status.attachLatencyMillis}' 2>/dev/null)
  guestBDF=$(kubectl get vmgpuattachment -n "$NS" "$POD" -o jsonpath='{.status.guestBDF}' 2>/dev/null)

  uid=$(kubectl logs -n "$NS" "$POD" -c worker --tail=800 2>/dev/null \
    | grep "\"ate.actor.name\":\"$ACTOR\"" | grep -o '"ate.actor.uid":"[^"]*"' | head -1 | cut -d'"' -f4)
  bootphases=$(kubectl logs -n "$NS" "$POD" -c worker --tail=800 2>/dev/null \
    | grep "\"id\":\"$uid\"" | grep "Actor boot phases")

  t0=$(date +%s%N)
  kubectl-ate suspend actor "$ACTOR" -a "$NS" >/dev/null 2>&1
  suspend_cmd_wall_ms=$(( ($(date +%s%N) - t0) / 1000000 ))

  detach_poll_wall_ms=$(wait_phase "Detached" 15)
  reconciler_detachMs=$(kubectl get vmgpuattachment -n "$NS" "$POD" -o jsonpath='{.status.attachLatencyMillis}' 2>/dev/null)

  echo "resume_cmd_wall_ms=$resume_cmd_wall_ms attach_poll_wall_ms=$attach_poll_wall_ms reconciler_attachMs=$reconciler_attachMs guestBDF=$guestBDF"
  echo "suspend_cmd_wall_ms=$suspend_cmd_wall_ms detach_poll_wall_ms=$detach_poll_wall_ms reconciler_detachMs=$reconciler_detachMs"
  echo "boot_phases=$bootphases"

  kubectl-ate delete actor "$ACTOR" -a "$NS" >/dev/null 2>&1
  echo
done
