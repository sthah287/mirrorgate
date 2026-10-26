#!/usr/bin/env bash
# What mirroring costs the user, measured rather than assumed.
#
#   ./experiments/run-overhead.sh
#
# Two arms only: mirroring off and mirroring everything, at a higher request
# rate than the sampling experiment so that if the shadow work is going to
# show up in client latency, it has a chance to. Everything else is identical
# between the two, including the container limits and the reset.
#
# "The candidate request runs in a goroutine" is not evidence on its own, so
# this produces numbers with intervals around them instead.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/overhead
mkdir -p "$OUT"

TRIALS=${TRIALS:-5}
RATE=${RATE:-500}
DURATION=${DURATION:-20s}
PUBLISH_MODE=${PUBLISH_MODE:-outbox}

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

wait_for_pipeline() {
  local last=-1 same=0 pending stored
  for _ in $(seq 1 180); do
    pending=$(psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r')
    stored=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
    if [[ "$pending" == "0" && "$stored" == "$last" ]]; then
      same=$((same + 1))
      (( same >= 3 )) && return 0
    else
      same=0
    fi
    last=$stored
    sleep 1
  done
}

reset_state() {
  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
  psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d stable_app -qc "TRUNCATE orders" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d candidate_sandbox -qc "TRUNCATE orders" >/dev/null
  curl -sf -X POST http://localhost:9003/reset >/dev/null
}

run_one() {
  local name=$1 mode=$2 rate=$3 trial=$4
  local run="$name-t$trial"

  echo
  echo "=============================================================="
  echo " $run  (mirroring $name, $RATE req/s for $DURATION)"
  echo "=============================================================="

  reset_state
  PUBLISH_MODE="$PUBLISH_MODE" SAMPLING_MODE="$mode" SAMPLING_RATE="$rate" SAMPLING_RULES="" \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway

  local stopfile="$OUT/.$run.stop"
  rm -f "$stopfile"
  python experiments/resource_monitor.py "$OUT/$run.resources.json" "$stopfile" &
  local monitor=$!
  sleep 1

  docker compose run --rm -T \
    -e RUN_NAME="$run" -e RATE="$RATE" -e DURATION="$DURATION" -e RESULT_DIR=overhead \
    k6-rare

  touch "$stopfile"
  wait "$monitor" 2>/dev/null || true
  rm -f "$stopfile"

  wait_for_pipeline
  curl -s http://localhost:8081/internal/stats > "$OUT/$run.stats.json"
  python experiments/collect_overhead.py "$run" "$OUT" "$RATE" "$DURATION"
  rm -f "$OUT/$run.stats.json" "$OUT/$run.k6.json" "$OUT/$run.resources.json"
}

for trial in $(seq 1 "$TRIALS"); do
  echo
  echo "##################### trial $trial of $TRIALS #####################"
  run_one off random 0 "$trial"
  run_one full full "" "$trial"
done

echo
echo "measuring the latency difference the comparison rules call significant"
python experiments/slow_threshold.py "$OUT"

python experiments/summarize_overhead.py "$OUT"

docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT"
