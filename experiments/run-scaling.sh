#!/usr/bin/env bash
# Does a second comparison worker actually share the work?
#
#   ./experiments/run-scaling.sh            one worker, then two
#   ./experiments/run-scaling.sh 2          just the two-worker run
#   REBALANCE=1 ./experiments/run-scaling.sh rebalance
#
# The comparison workers are one consumer group, so the topic's partitions
# are what decides how many of them can do anything. This runs the same
# workload at each size and records, per worker, how many events it actually
# processed, taken from the workers' own logs rather than inferred.
#
# The rebalance run adds the second worker while traffic is still arriving,
# which is the case that can lose or duplicate events if the group handles it
# badly. At-least-once delivery means a duplicate delivery is allowed; a
# duplicate stored comparison is not, and neither is a missing one.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/pr4
mkdir -p "$OUT"
RATE=${RATE:-100}
DURATION=${DURATION:-30s}
TOPIC=${TOPIC:-mirrorgate.comparisons}
GROUP=${GROUP:-mirrorgate-comparison-worker}

psql_query() { docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; }
pending() { psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r'; }
stored() { psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r'; }
rpk() { docker compose exec -T redpanda rpk "$@" 2>/dev/null; }

wait_for_gateway() {
  for _ in $(seq 1 60); do
    curl -sf http://localhost:8081/internal/health >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "gateway did not come back" >&2
  exit 1
}

reset_state() {
  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
  psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d stable_app -qc "TRUNCATE orders" >/dev/null
  docker compose exec -T postgres psql -U mirrorgate -d candidate_sandbox -qc "TRUNCATE orders" >/dev/null
  curl -sf -X POST http://localhost:9003/reset >/dev/null
}

# Recreated before every run so the logs below only cover this run.
scale_workers() {
  docker compose up -d --build --force-recreate --scale comparison-worker="$1" comparison-worker >/dev/null 2>&1
  sleep 5
}

wait_for_drain() {
  local last=-1 same=0 now
  for _ in $(seq 1 180); do
    now=$(stored)
    if [[ "$(pending)" == "0" && "$now" == "$last" ]]; then
      same=$((same + 1))
      (( same >= 3 )) && return 0
    else
      same=0
    fi
    last=$now
    sleep 1
  done
  echo "pipeline did not drain in time" >&2
}

run_one() {
  local workers=$1 label=$2 rebalance=${3:-0}

  echo
  echo "=============================================================="
  echo " $label"
  echo "=============================================================="
  reset_state
  if [[ "$rebalance" == "1" ]]; then
    scale_workers 1
  else
    scale_workers "$workers"
  fi

  rpk topic describe "$TOPIC" | grep -iE "^(NAME|PARTITIONS)" | sed 's/^/  /' || true
  echo "  partitions: $(rpk topic describe -p "$TOPIC" | grep -c '^[0-9]' || true)"

  PUBLISH_MODE=outbox SAMPLING_MODE=full SAMPLING_RATE= SAMPLING_RULES= \
    docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway

  # Anything already in the topic from an earlier run would be consumed
  # during this one and counted as this run's work, so wait for the group
  # to catch up first, and start from a gateway whose counters are zero.
  for _ in $(seq 1 60); do
    lag=$(rpk group describe "$GROUP" | awk '$1 == "TOTAL-LAG" {print $2}')
    [[ "$lag" == "0" ]] && break
    sleep 1
  done
  eligible=$(curl -s http://localhost:8081/internal/stats |
    python -c "import sys,json;print(json.load(sys.stdin)['gateway']['eligible'])")
  if [[ "$eligible" != "0" ]]; then
    echo "the gateway had already seen $eligible requests before the load started" >&2
    exit 1
  fi
  echo "  consumer group caught up, gateway counters at zero"

  local stopfile="$OUT/.$label.stop"
  rm -f "$stopfile"
  python experiments/resource_monitor.py "$OUT/$label.resources.json" "$stopfile" &
  local monitor=$!
  sleep 1

  if [[ "$rebalance" == "1" ]]; then
    (
      sleep 10
      echo "  adding the second worker mid-run"
      docker compose up -d --no-recreate --scale comparison-worker=2 comparison-worker >/dev/null 2>&1
    ) &
  fi

  docker compose run --rm -T \
    -e RUN_NAME="$label" -e RATE="$RATE" -e DURATION="$DURATION" -e RESULT_DIR=pr4 \
    k6-rare >/dev/null

  echo "  load finished, waiting for the pipeline"
  wait_for_drain
  touch "$stopfile"
  wait "$monitor" 2>/dev/null || true
  rm -f "$stopfile"

  docker compose logs --no-color comparison-worker > "$OUT/.$label.worker.log" 2>&1
  rpk group describe "$GROUP" > "$OUT/.$label.group.txt" 2>&1 || true
  curl -s http://localhost:8081/internal/stats > "$OUT/.$label.stats.json"
  psql_query -tAc "
    SELECT json_build_object(
      'stored', count(*),
      'distinct_request_ids', count(DISTINCT request_id)
    ) FROM request_comparisons" | tr -d '\r' > "$OUT/.$label.db.json"

  python experiments/collect_scaling.py "$label" "$OUT" "$workers" "$rebalance"
  rm -f "$OUT/.$label.worker.log" "$OUT/.$label.group.txt" "$OUT/.$label.stats.json" \
        "$OUT/.$label.db.json" "$OUT/$label.resources.json" "$OUT/$label.k6.json"
}

which=${1:-all}
case "$which" in
  1)         run_one 1 scaling-1worker ;;
  2)         run_one 2 scaling-2workers ;;
  rebalance) run_one 2 scaling-rebalance 1 ;;
  all)
    run_one 1 scaling-1worker
    run_one 2 scaling-2workers
    run_one 2 scaling-rebalance 1
    ;;
  *) echo "usage: $0 [1|2|rebalance|all]" >&2; exit 1 ;;
esac

# Back to one worker and the default gateway.
docker compose up -d --force-recreate --scale comparison-worker=1 comparison-worker >/dev/null 2>&1
docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "results in $OUT, stack back to one worker"
