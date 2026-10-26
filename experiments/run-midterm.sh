#!/usr/bin/env bash
# Midterm experiment: the same workload against every sampling strategy,
# repeated five times.
#
#   ./experiments/run-midterm.sh            everything (about 40 minutes)
#   ./experiments/run-midterm.sh risk       one strategy, still 5 trials
#   TRIALS=1 ./experiments/run-midterm.sh   a quick check
#
# Every run uses the same workload (experiments/midterm.js), the same request
# rate and duration, the same candidate services, the same container CPU and
# memory limits from docker-compose.yml, and the same reset procedure. The
# only thing that changes between runs is SAMPLING_MODE and its settings.
#
# Trials are the outer loop, so if the machine slows down partway through,
# it affects every strategy roughly equally instead of ruining one of them.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/midterm
mkdir -p "$OUT"

TRIALS=${TRIALS:-5}
RATE=${RATE:-100}
DURATION=${DURATION:-30s}

# name : SAMPLING_MODE : SAMPLING_RATE : SAMPLING_RULES
STRATEGIES=(
  "full:full::"
  "random-50:random:0.5:"
  "random-25:random:0.25:"
  "random-10:random:0.1:"
  "endpoint:endpoint:0:/api/products*=0.5,/api/users*=0.25,/api/orders*=0.5,/health=0"
  "risk:risk::"
)

psql_query() {
  docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"
}

wait_for_gateway() {
  for _ in $(seq 1 60); do
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
  local last=-1 same=0 count
  for _ in $(seq 1 90); do
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
  echo "worker never finished draining the topic" >&2
}

# Identical before every single run: no stored comparisons, no leftover
# orders on either side, no recorded external calls, and a gateway that has
# just started so its counters and its risk state are empty.
reset_state() {
  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
  psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d stable_app -qc "TRUNCATE orders" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d candidate_sandbox -qc "TRUNCATE orders" >/dev/null
  curl -sf -X POST http://localhost:9003/reset >/dev/null
}

run_one() {
  local name=$1 mode=$2 rate=$3 rules=$4 trial=$5
  local run="$name-t$trial"

  echo
  echo "=============================================================="
  echo " $run  (mode=$mode rate=${rate:-n/a} rules=${rules:-none})"
  echo "=============================================================="

  # Clear first, restart second. Risk mode reads the comparison history the
  # moment it starts, so a gateway restarted before the truncate would spend
  # the first seconds of the run scored on the previous trial's results.
  reset_state
  SAMPLING_MODE="$mode" SAMPLING_RATE="$rate" SAMPLING_RULES="$rules" \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway
  echo "state cleared, gateway restarted cold"

  # The script path lives in docker-compose.yml, not here: Git Bash on
  # Windows rewrites a bare /scripts/... argument into a Windows path.
  docker compose run --rm -T \
    -e RUN_NAME="$run" -e RATE="$RATE" -e DURATION="$DURATION" \
    k6-midterm

  echo "waiting for the worker to drain the topic"
  wait_for_worker

  curl -s http://localhost:8081/internal/stats > "$OUT/$run.stats.json"
  curl -s http://localhost:8081/internal/sampling > "$OUT/$run.sampling.json"
  curl -s http://localhost:9003/calls > "$OUT/$run.external.json"

  # Detection times are measured from the first mirrored request rather than
  # from the moment k6 started, so everything comes from one clock. At 100
  # requests a second that start point is within a few hundred milliseconds
  # of the real one.
  psql_query -tAc "
    WITH run AS (SELECT min(received_at) AS started FROM request_comparisons)
    SELECT json_build_object(
      'stored',    count(*),
      'matched',   count(*) FILTER (WHERE outcome = 'match'),
      'different', count(*) FILTER (WHERE outcome = 'different'),
      'errors',    count(*) FILTER (WHERE outcome = 'error'),
      'slow',      count(*) FILTER (WHERE candidate_slow),
      'orders_mirrored', count(*) FILTER (WHERE method = 'POST' AND path = '/api/orders'),
      'r1_hits', count(*) FILTER (WHERE path = '/api/products/12' AND outcome = 'different'),
      'r2_hits', count(*) FILTER (WHERE path = '/api/products/8'  AND outcome = 'error'),
      'r3_hits', count(*) FILTER (WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow),
      'r4_hits', count(*) FILTER (WHERE path = '/api/users/7'   AND outcome = 'error'),
      'r5_hits', count(*) FILTER (WHERE path = '/api/orders' AND method = 'POST' AND outcome = 'different'),
      'r1_first_s', EXTRACT(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products/12' AND outcome = 'different') - (SELECT started FROM run))),
      'r2_first_s', EXTRACT(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products/8'  AND outcome = 'error')     - (SELECT started FROM run))),
      'r3_first_s', EXTRACT(epoch FROM (min(received_at) FILTER (WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow) - (SELECT started FROM run))),
      'r4_first_s', EXTRACT(epoch FROM (min(received_at) FILTER (WHERE path = '/api/users/7'   AND outcome = 'error')       - (SELECT started FROM run))),
      'r5_first_s', EXTRACT(epoch FROM (min(received_at) FILTER (WHERE path = '/api/orders' AND method = 'POST' AND outcome = 'different') - (SELECT started FROM run)))
    )
    FROM request_comparisons" | tr -d '\r' > "$OUT/$run.db.json"

  python experiments/collect_midterm.py "$run" "$OUT" "$RATE" "$DURATION" "$mode"
  rm -f "$OUT/$run.stats.json" "$OUT/$run.db.json" "$OUT/$run.k6.json" \
        "$OUT/$run.sampling.json" "$OUT/$run.external.json"
}

only=${1:-}
for trial in $(seq 1 "$TRIALS"); do
  echo
  echo "##################### trial $trial of $TRIALS #####################"
  for entry in "${STRATEGIES[@]}"; do
    IFS=: read -r name mode rate rules <<< "$entry"
    if [[ -n "$only" && "$only" != "$name" ]]; then
      continue
    fi
    run_one "$name" "$mode" "$rate" "$rules" "$trial"
  done
done

python experiments/summarize_midterm.py "$OUT"

# Put the gateway back on the default configuration.
docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT"
