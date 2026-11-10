#!/usr/bin/env bash
# Rare-fault experiment: the same workload against every sampling strategy,
# repeated, with the faults rare enough that strategies actually miss them.
#
#   ./experiments/run-rare.sh            everything (about 90 minutes)
#   ./experiments/run-rare.sh risk,risk-urls   just those arms, all trials
#   TRIALS=1 ./experiments/run-rare.sh         a quick check
#   OUT=results/pr4/rare ./experiments/run-rare.sh   write somewhere else
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
# Overridable so a later milestone can run the same experiment without
# writing into an earlier one's results.
OUT=${OUT:-results/rare-faults}
mkdir -p "$OUT"
# k6 writes its summary from inside the container, where the results
# directory is mounted at /results, so it needs the path without the prefix.
RESULT_DIR=${OUT#results/}

TRIALS=${TRIALS:-10}
# Where to start. A four hour run that dies on a transient Docker error
# should be resumable without redoing the trials that finished.
FIRST_TRIAL=${FIRST_TRIAL:-1}
RATE=${RATE:-100}
DURATION=${DURATION:-30s}
# k6-rare puts the faults at fixed slots. k6-rare-random draws their
# positions from SEED_BASE plus the trial number, so every strategy in a
# trial sees the same positions and the next trial sees different ones.
WORKLOAD=${WORKLOAD:-k6-rare}
SEED_BASE=${SEED_BASE:-}
# The outbox is the point of week 8, and it also means a missed fault is a
# sampling decision rather than a lost event.
PUBLISH_MODE=${PUBLISH_MODE:-outbox}

# name : SAMPLING_MODE : SAMPLING_RATE : SAMPLING_RULES : RISK_ROUTE_TEMPLATES
# off mirrors nothing. It detects nothing by definition and is here as the
# cost and latency baseline.
ARMS=(
  "off:random:0::"
  "full:full:::"
  "random-50:random:0.5::"
  "random-25:random:0.25::"
  "random-10:random:0.1::"
  "endpoint:endpoint:0:/api/products*=0.5,/api/users*=0.25,/api/orders*=0.5,/health=0:"
  "risk:risk:::true"
  "risk-urls:risk:::false"
)

# Retried: Docker Desktop's API occasionally answers one call with a 500,
# and losing a whole experiment to that is worse than trying again.
psql_query() {
  local attempt
  for attempt in 1 2 3; do
    if docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; then
      return 0
    fi
    sleep 2
  done
  return 1
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
  local name=$1 mode=$2 rate=$3 rules=$4 templates=$5 trial=$6
  local run="$name-t$trial"

  echo
  echo "=============================================================="
  echo " $run  (mode=$mode rate=${rate:-n/a} publish=$PUBLISH_MODE)"
  echo "=============================================================="

  # Clear first, restart second. Risk mode reads the comparison history as
  # soon as it starts, so a gateway restarted before the truncate would spend
  # the first seconds of the run scored on the previous trial's results.
  reset_state
  PUBLISH_MODE="$PUBLISH_MODE" SAMPLING_MODE="$mode" SAMPLING_RATE="$rate" \
    SAMPLING_RULES="$rules" RISK_ROUTE_TEMPLATES="$templates" \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway
  # The image carries the sampling code, so a rebuild has to happen before
  # the arms are compared. Ask the gateway what it is actually running
  # rather than trusting that the variable reached it.
  if [[ "$mode" == "risk" ]]; then
    local want=${templates:-true} got
    got=$(curl -s http://localhost:8081/internal/sampling |
      python -c "import sys,json;print(str(json.load(sys.stdin)['sampling']['route_templates']).lower())")
    if [[ "$got" != "$want" ]]; then
      echo "gateway reports route_templates=$got, expected $want" >&2
      exit 1
    fi
  fi
  echo "state cleared, gateway restarted cold"

  # The seed belongs to the trial, not to the arm.
  local seed="" total=""
  if [[ -n "$SEED_BASE" ]]; then
    seed=$((SEED_BASE + trial))
    if [[ "$DURATION" != *s ]]; then
      echo "SEED_BASE needs DURATION in seconds, like 30s" >&2
      exit 1
    fi
    total=$(( RATE * ${DURATION%s} ))
  fi

  local stopfile="$OUT/.$run.stop"
  rm -f "$stopfile"
  python experiments/resource_monitor.py "$OUT/$run.resources.json" "$stopfile" &
  local monitor=$!
  sleep 1

  docker compose run --rm -T \
    -e RUN_NAME="$run" -e RATE="$RATE" -e DURATION="$DURATION" -e RESULT_DIR="$RESULT_DIR" \
    -e SEED="$seed" -e TOTAL="$total" \
    "$WORKLOAD"

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
        'R1', (SELECT json_build_object('hits', count(*), 'seqs', json_agg(request_seq ORDER BY request_seq), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products/12' AND outcome = 'different'),
        'R2', (SELECT json_build_object('hits', count(*), 'seqs', json_agg(request_seq ORDER BY request_seq), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products/8' AND outcome = 'error'),
        'R3', (SELECT json_build_object('hits', count(*), 'seqs', json_agg(request_seq ORDER BY request_seq), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow),
        'R4', (SELECT json_build_object('hits', count(*), 'seqs', json_agg(request_seq ORDER BY request_seq), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
               FROM request_comparisons WHERE path = '/api/users/7' AND outcome = 'error'),
        'R5', (SELECT json_build_object('hits', count(*), 'seqs', json_agg(request_seq ORDER BY request_seq), 'first_seq', min(request_seq), 'first_received_at', min(received_at), 'first_detected_at', min(detected_at))
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
  # The fault plan is kept inside the run file, so the intermediate copy goes.
  rm -f "$OUT/$run.stats.json" "$OUT/$run.db.json" "$OUT/$run.k6.json" \
        "$OUT/$run.sampling.json" "$OUT/$run.external.json" "$OUT/$run.resources.json" \
        "$OUT/$run.faults.json"
}

# Built once: every arm has to run the same binary, and --force-recreate
# alone would quietly keep an older image.
docker compose build gateway >/dev/null

only=${1:-}
for trial in $(seq "$FIRST_TRIAL" "$TRIALS"); do
  echo
  echo "##################### trial $trial of $TRIALS #####################"
  for entry in "${ARMS[@]}"; do
    IFS=: read -r name mode rate rules templates <<< "$entry"
    if [[ -n "$only" && ",$only," != *",$name,"* ]]; then
      continue
    fi
    run_one "$name" "$mode" "$rate" "$rules" "$templates" "$trial"
  done
done

python experiments/summarize_rare.py "$OUT"

# Put the gateway back on the defaults.
docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT"
