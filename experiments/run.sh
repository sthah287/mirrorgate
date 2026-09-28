#!/usr/bin/env bash
# Runs the sampling experiment: the same k6 workload against each sampling
# strategy, with the gateway restarted in between so its counters start at
# zero and the new configuration is picked up.
#
#   ./experiments/run.sh              all strategies
#   ./experiments/run.sh random-25    just one
#
# Results land in results/progress-report-2/.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/progress-report-2
mkdir -p "$OUT"

# Set SUFFIX to keep a side experiment (a different request rate, say)
# from overwriting the main per-strategy result files.
SUFFIX=${SUFFIX:-}

# name : SAMPLING_MODE : SAMPLING_RATE : SAMPLING_RULES
STRATEGIES=(
  "off:random:0:"
  "full:full::"
  "random-50:random:0.5:"
  "random-25:random:0.25:"
  "random-10:random:0.1:"
  "endpoint:endpoint:0:/api/products/*=0.5,/api/users/*=0.25,/health=0"
  # Same intent, but the patterns drop the slash so they also cover the
  # collection endpoints (/api/products as well as /api/products/12).
  "endpoint-fixed:endpoint:0:/api/products*=0.5,/api/users*=0.25,/health=0"
)

psql_query() {
  docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"
}

wait_for_gateway() {
  for _ in $(seq 1 40); do
    if curl -sf http://localhost:8081/internal/health >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "gateway did not come back up" >&2
  exit 1
}

# The worker processes events after k6 has already finished, so wait until the
# row count stops moving before reading any results.
wait_for_worker() {
  local last=-1 same=0
  for _ in $(seq 1 60); do
    local count
    count=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
    if [[ "$count" == "$last" ]]; then
      same=$((same + 1))
      if (( same >= 3 )); then
        return 0
      fi
    else
      same=0
    fi
    last=$count
    sleep 1
  done
}

run_one() {
  local name=$1$SUFFIX mode=$2 rate=$3 rules=$4

  echo
  echo "=============================================================="
  echo " $name  (mode=$mode rate=${rate:-default} rules=${rules:-none})"
  echo "=============================================================="

  SAMPLING_MODE="$mode" SAMPLING_RATE="$rate" SAMPLING_RULES="$rules" \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway

  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
  echo "gateway restarted, comparisons cleared"

  # The script path lives in docker-compose.yml, not here: Git Bash on Windows
  # rewrites a bare /scripts/... argument into a Windows path.
  docker compose run --rm -T \
    -e RUN_NAME="$name" -e RATE="${RATE:-100}" -e DURATION="${DURATION:-30s}" \
    k6

  echo "waiting for the worker to drain the topic"
  wait_for_worker

  curl -s http://localhost:8081/internal/stats > "$OUT/$name.stats.json"
  psql_query -tAc "
    SELECT json_build_object(
      'stored', count(*),
      'matched',   count(*) FILTER (WHERE outcome = 'match'),
      'different', count(*) FILTER (WHERE outcome = 'different'),
      'errors',    count(*) FILTER (WHERE outcome = 'error'),
      'slow',      count(*) FILTER (WHERE candidate_slow),
      'price_regression',   count(*) FILTER (WHERE path = '/api/products/12' AND outcome = 'different'),
      'status_regression',  count(*) FILTER (WHERE path = '/api/products/8'  AND outcome = 'error'),
      'slow_regression',    count(*) FILTER (WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow),
      'timeout_regression', count(*) FILTER (WHERE path = '/api/users/7'   AND outcome = 'error'),
      'first_price_after_s',   round(extract(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products/12' AND outcome = 'different') - min(received_at)))::numeric, 1),
      'first_status_after_s',  round(extract(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products/8'  AND outcome = 'error')     - min(received_at)))::numeric, 1),
      'first_slow_after_s',    round(extract(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow) - min(received_at)))::numeric, 1),
      'first_timeout_after_s', round(extract(epoch FROM (min(received_at) FILTER (WHERE path = '/api/users/7'   AND outcome = 'error')       - min(received_at)))::numeric, 1)
    )
    FROM request_comparisons" | tr -d '\r' > "$OUT/$name.db.json"

  python experiments/collect.py "$name" "$OUT"
  rm -f "$OUT/$name.stats.json" "$OUT/$name.db.json" "$OUT/$name.k6.json"
}

only=${1:-}
for entry in "${STRATEGIES[@]}"; do
  IFS=: read -r name mode rate rules <<< "$entry"
  if [[ -n "$only" && "$only" != "$name" ]]; then
    continue
  fi
  run_one "$name" "$mode" "$rate" "$rules"
done

if [[ -z "$only" ]]; then
  python experiments/summarize.py "$OUT"
fi

# Put the gateway back on the default configuration.
docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT"
