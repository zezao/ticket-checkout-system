#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

scenario="${1:-uniform}"
vus="${BENCH_VUS:-50}"
duration="${BENCH_DURATION:-30s}"
event_id="${BENCH_EVENT_ID:-0199b000-0000-7000-8000-000000000001}"
base="${BENCH_BASE_URL:-http://127.0.0.1:8080}"

case "$scenario" in
  hot|uniform|skewed) script="hold.js" ;;
  seatmap) script="seatmap.js" ;;
  *)
    echo "usage: $0 hot|uniform|skewed|seatmap" >&2
    exit 2
    ;;
esac

if [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$base/readyz" || true)" != "200" ]]; then
  echo "API is not ready at $base; run 'docker compose up -d --build' first." >&2
  exit 1
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
out="docs/benchmarks/raw/${scenario}-${stamp}"
mkdir -p "$out"

echo "resetting benchmark event ${event_id}"
BENCH_EVENT_ID="$event_id" make bench-seed

cat > "$out/meta.txt" <<EOF
scenario=${scenario}
commit=${commit}
vus=${vus}
duration=${duration}
event_id=${event_id}
started_at=${stamp}
host=$(uname -srm)
EOF

pg_id="$(docker compose ps -q postgres)"
monitor="$out/monitor.csv"
printf 'timestamp,acquired_conns,pool_wait_seconds,outbox_lag_seconds,postgres_cpu_percent\n' > "$monitor"

(
  while true; do
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    api="$(curl -s --max-time 1 "$base/metrics" 2>/dev/null || true)"
    worker="$(curl -s --max-time 1 http://127.0.0.1:9090/metrics 2>/dev/null || true)"
    acquired="$(printf '%s\n' "$api" | awk '/^pgxpool_acquired_conns/ && /pool="app"/ {print $2; exit}')"
    wait="$(printf '%s\n' "$api" | awk '/^pgxpool_acquire_wait_seconds_total/ && /pool="app"/ {print $2; exit}')"
    lag="$(printf '%s\n' "$worker" | awk '/^outbox_lag_seconds / {print $2; exit}')"
    cpu="$(docker stats --no-stream --format '{{.CPUPerc}}' "$pg_id" 2>/dev/null | tr -d '%' || true)"
    printf '%s,%s,%s,%s,%s\n' "$ts" "${acquired:-}" "${wait:-}" "${lag:-}" "${cpu:-}" >> "$monitor"
    sleep 0.5
  done
) &
monitor_pid=$!
trap 'kill "$monitor_pid" 2>/dev/null || true' EXIT INT TERM

echo "running ${scenario}: ${vus} VUs for ${duration}"
set +e
docker compose --profile bench run --rm \
  -e "SCENARIO=${scenario}" -e "VUS=${vus}" -e "DURATION=${duration}" \
  k6 run "/scripts/${script}" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tee "$out/k6.txt"
k6_status=${PIPESTATUS[0]}
set -e

kill "$monitor_pid" 2>/dev/null || true
wait "$monitor_pid" 2>/dev/null || true
trap - EXIT INT TERM

awk -F, '
  NR == 1 { next }
  {
    samples++
    if ($2 != "" && $2 + 0 > maxAcq) maxAcq = $2 + 0
    if ($3 != "") {
      if (firstWait == "") firstWait = $3 + 0
      lastWait = $3 + 0
    }
    if ($4 != "" && $4 + 0 > maxLag) maxLag = $4 + 0
    if ($5 != "") {
      cpuSum += $5 + 0
      if ($5 + 0 > maxCpu) maxCpu = $5 + 0
      cpuSamples++
    }
  }
  END {
    printf "samples=%d\n", samples
    printf "pool_max_acquired=%d\n", maxAcq
    if (firstWait != "") printf "pool_wait_seconds_delta=%.3f\n", lastWait - firstWait
    printf "outbox_lag_max_seconds=%.3f\n", maxLag
    if (cpuSamples > 0) {
      printf "postgres_cpu_max_percent=%.2f\n", maxCpu
      printf "postgres_cpu_avg_percent=%.2f\n", cpuSum / cpuSamples
    }
  }
' "$monitor" | tee "$out/monitor-summary.txt"

echo "raw results: ${out}"
exit "$k6_status"
