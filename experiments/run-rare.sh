#!/usr/bin/env bash
# Rare-fault experiment: the same workload against every sampling strategy,
# repeated, with the faults rare enough that strategies actually miss them.
#
#   ./experiments/run-rare.sh            everything (about 90 minutes)
#   ./experiments/run-rare.sh risk       one arm, still all trials
#   TRIALS=1 ./experiments/run-rare.sh   a quick check
#
# Held constant across every run: the workload (experiments/rare.js), the
# request rate and duration, the faults, the container CPU and memory limits
# from docker-compose.yml, and the reset below. The only thing that changes is
# the sampling configuration.
#
# Trials are the outer loop, so if the machine slows down partway through it
# affects every arm roughly equally instead of ruining one of them.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/rare-faults
mkdir -p "$OUT"

TRIALS=${TRIALS:-10}
RATE=${RATE:-100}
DURATION=${DURATION:-30s}
# The outbox is the point of week 8, and it also means a missed fault is a
# sampling decision rather than a lost event.
PUBLISH_MODE=${PUBLISH_MODE:-outbox}

# name : SAMPLING_MODE : SAMPLING_RATE : SAMPLING_RULES
# off mirrors nothing. It detects nothing by definition and is here as the
# cost and latency baseline.
ARMS=(
  "off:random:0:"
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
    curl -sf http://localhost:8081/internal/health >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "gateway did not come back up" >&2
  exit 1
}

# Two queues drain after k6 finishes: the outbox and the topic. Wait for both
# to go quiet before reading any results.
wait_for_pipeline() {
  local last=-1 same=0 pending stored
  for _ in $(seq 1 120); do
    pending=$(psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r')
    stored=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
    if [[ "$pending" == "0" && "$stored" == "$last" ]]; then
      same=$((same + 1))
      if (( same >= 3 )); then
        return 0
      fi
    else
      same=0
    fi
    last=$stored
    sleep 1
  done
  echo "pipeline did not drain in time" >&2
}

# Identical before every run.
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
  echo " $run  (mode=$mode rate=${rate:-n/a} publish=$PUBLISH_MODE)"
  echo "=============================================================="

  # Clear first, restart second. Risk mode reads the comparison history as
  # soon as it starts, so a gateway restarted before the truncate would spend
  # the first seconds of the run scored on the previous trial's results.
  reset_state
  PUBLISH_MODE="$PUBLISH_MODE" SAMPLING_MODE="$mode" SAMPLING_RATE="$rate" SAMPLING_RULES="$rules" \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway
  echo "state cleared, gateway restarted cold"

  local stopfile="$OUT/.$run.stop"
  rm -f "$stopfile"
  python experiments/resource_monitor.py "$OUT/$run.resources.json" "$stopfile" &
  local monitor=$!
  sleep 1

  docker compose run --rm -T \
    -e RUN_NAME="$run" -e RATE="$RATE" -e DURATION="$DURATION" \
    k6-rare

  touch "$stopfile"
  wait "$monitor" 2>/dev/null || true
  rm -f "$stopfile"

  echo "waiting for the outbox and the topic to drain"
  wait_for_pipeline

  curl -s http://localhost:8081/internal/stats > "$OUT/$run.stats.json"
  curl -s http://localhost:8081/internal/sampling > "$OUT/$run.sampling.json"
  curl -s http://localhost:9003/calls > "$OUT/$run.external.json"

  # Exact detection data. request_seq is the position the gateway assigned to
  # the request, counted from its own restart, so "requests until detection"
  # is a counted integer rather than elapsed time multiplied by a rate.
  # received_at is the gateway clock, which is the same clock started_at in
  # the stats file comes from, so the two can be subtracted safely.
  psql_query -tAc "
    SELECT json_build_object(
      'stored',    count(*),
      'matched',   count(*) FILTER (WHERE outcome = 'match'),
      'different', count(*) FILTER (WHERE outcome = 'different'),
      'errors',    count(*) FILTER (WHERE outcome = 'error'),
      'slow',      count(*) FILTER (WHERE candidate_slow),
      'orders_mirrored', count(*) FILTER (WHERE method = 'POST' AND path = '/api/orders'),
      'max_seq',   COALESCE(max(request_seq), 0),
      -- Anchor for the time metrics. Measuring from the gateway process
      -- start would fold in the seconds between the restart and k6 actually
      -- sending traffic, which is not part of detecting anything.
      -- received_at is the gateway clock, detected_at the worker clock. Both
      -- containers read the same VM kernel clock, so they are comparable, but
      -- they are different processes and are reported separately.
      'run_first_received_at', min(received_at),
      'faults', json_build_object(
        'R1', (SELECT json_build_object('hits', count(*), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products/12' AND outcome = 'different'),
        'R2', (SELECT json_build_object('hits', count(*), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products/8' AND outcome = 'error'),
        'R3', (SELECT json_build_object('hits', count(*), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow),
        'R4', (SELECT json_build_object('hits', count(*), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/users/7' AND outcome = 'error'),
        'R5', (SELECT json_build_object('hits', count(*), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/orders' AND method = 'POST' AND outcome = 'different')
      ),
      'clean_latency_deltas', (
        SELECT COALESCE(json_agg(round((candidate_latency_ms - stable_latency_ms)::numeric, 3)), '[]'::json)
        FROM request_comparisons
        WHERE candidate_error IS NULL
          AND NOT (path = '/api/products' AND query = 'category=audio')
          AND path NOT IN ('/api/products/12', '/api/products/8', '/api/users/7', '/api/orders')
      )
    )
    FROM request_comparisons" | tr -d '\r' > "$OUT/$run.db.json"

  python experiments/collect_rare.py "$run" "$OUT" "$RATE" "$DURATION" "$mode" "$PUBLISH_MODE"
  rm -f "$OUT/$run.stats.json" "$OUT/$run.db.json" "$OUT/$run.k6.json" \
        "$OUT/$run.sampling.json" "$OUT/$run.external.json" "$OUT/$run.resources.json"
}

only=${1:-}
for trial in $(seq 1 "$TRIALS"); do
  echo
  echo "##################### trial $trial of $TRIALS #####################"
  for entry in "${ARMS[@]}"; do
    IFS=: read -r name mode rate rules <<< "$entry"
    if [[ -n "$only" && "$only" != "$name" ]]; then
      continue
    fi
    run_one "$name" "$mode" "$rate" "$rules" "$trial"
  done
done

python experiments/summarize_rare.py "$OUT"

# Put the gateway back on the defaults.
docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT"
