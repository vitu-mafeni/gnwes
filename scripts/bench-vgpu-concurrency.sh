# Repeated-cycle benchmark for the Shared-tier Volcano vGPU demo
# (config/substrate-integration/gnwes-counter-vgpu-{pool.yaml,
# workerpool.yaml.tmpl,template.yaml.tmpl}): drives N suspend+resume
# cycles across all 3 gnwes-actor-vgpu-* actors at once and records, per
# cycle, each actor's real time-to-Running (the Kubernetes scheduling +
# Volcano admission + vgpu-device-plugin allocation latency -- there is
# no attach/detach step on this tier, binding happens once at pod
# creation, so this is the Shared-tier analogue of
# bench-attach-cycles.sh's attach/detach latency) and which physical GPU
# each one actually landed on.
#
# Exists because config/vmgpu/README.md's own "Metrics and success
# criteria" section calls out that a repeated test should report a
# range (p50/p95), not a single one-off number -- the 2026-11-15 proof
# that 2 of 3 actors shared one physical GPU was a single run, once.
#
# Run as: bash bench-vgpu-concurrency.sh <cycles>
set -uo pipefail
CYCLES="${1:-5}"
KA=/home/ubuntu/go/bin/kubectl-ate
NS=ate-demo-counter
ACTORS="gnwes-actor-vgpu-1 gnwes-actor-vgpu-2 gnwes-actor-vgpu-3"
POLL_INTERVAL_S=1
POLL_TIMEOUT_S=60
OUT=bench-vgpu-results.csv
echo "cycle,actor,phase,time_to_running_ms,worker_pod,gpu_uuid,timestamp" > "$OUT"

now_ms() { date +%s%3N; }

wait_for_running() {
  local actor="$1" start_ms="$2" deadline=$(( $(date +%s) + POLL_TIMEOUT_S ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local line
    line=$($KA get actor "$actor" -a "$NS" 2>/dev/null | awk -v a="$actor" '$2==a {print}')
    if echo "$line" | grep -q ACTOR_STATE_RUNNING; then
      local pod
      pod=$(echo "$line" | awk '{print $5}' | sed "s#^$NS/##")
      echo "$(( $(now_ms) - start_ms )) $pod"
      return 0
    fi
    sleep "$POLL_INTERVAL_S"
  done
  echo "TIMEOUT -"
  return 1
}

for i in $(seq 1 "$CYCLES"); do
  echo "== cycle $i/$CYCLES: suspend all 3 =="
  for a in $ACTORS; do $KA suspend actor "$a" -a "$NS"; done
  sleep 5

  echo "== cycle $i/$CYCLES: resume all 3, timing each to Running =="
  declare -A START_MS
  for a in $ACTORS; do
    START_MS[$a]=$(now_ms)
    $KA resume actor "$a" -a "$NS" >/dev/null
  done
  for a in $ACTORS; do
    read -r latency pod < <(wait_for_running "$a" "${START_MS[$a]}")
    gpu_uuid=""
    if [ -n "$pod" ] && [ "$pod" != "-" ]; then
      # The annotation's own value embeds a literal newline (observed
      # live: "GPU-<uuid>\nNVIDIA\n") -- extract just the UUID token so
      # it can't break this script's own CSV rows.
      gpu_uuid=$(kubectl get pod "$pod" -n "$NS" -o jsonpath='{.metadata.annotations.volcano\.sh/vgpu-ids-new}' 2>/dev/null | grep -oE 'GPU-[0-9a-f-]+' | head -1)
    fi
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    echo "$i,$a,resume,$latency,$pod,$gpu_uuid,$ts" >> "$OUT"
    echo "  $a: ${latency}ms -> $pod (gpu=$gpu_uuid)"
  done
done

echo
echo "== co-residency summary (last cycle) =="
tail -n 3 "$OUT" | awk -F, '{print $5, $6}' | sort | uniq -c | sort -rn

echo
echo "== latency distribution (ms) =="
awk -F, 'NR>1 && $4 != "TIMEOUT" {print $4}' "$OUT" | sort -n | awk '
  { a[NR]=$1 }
  END {
    n=NR
    if (n==0) { print "no samples"; exit }
    p50=a[int(n*0.5)+1]; p95=a[int(n*0.95)+1]
    printf "n=%d min=%d p50=%d p95=%d max=%d\n", n, a[1], p50, p95, a[n]
  }'

echo
echo "== full results: $OUT =="
cat "$OUT"
