#!/usr/bin/env bash
# Measure how fast the comparison pipeline actually keeps up.
#
#   SINGLE=true  ./experiments/relay-throughput.sh before-t1   one publish per row
#                ./experiments/relay-throughput.sh after-t1    batched publishing
#
# Runs the standard workload at full sampling and watches the outbox drain,
# which is where the backlog shows up. Writes results/pr4/relay-<label>.json.
#
# What it records:
#   offered requests and mirrored count
#   the outbox depth sampled twice a second for the whole run
#   peak backlog, and how long the queue took to clear after the load stopped
#   throughput during the drain
#   confirmation lag percentiles, from request arrival to the stored row
#   CPU seconds for the relay, worker and gateway
set -euo pipefail

cd "$(dirname "$0")/.."
LABEL=${1:-run}
OUT=results/pr4
mkdir -p "$OUT"
RATE=${RATE:-100}
DURATION=${DURATION:-30s}

psql_query() { docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; }
pending() { psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r'; }
stored() { psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r'; }

echo "resetting state"
psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
docker compose exec -T postgres psql -U mirrorgate -d stable_app -qc "TRUNCATE orders" >/dev/null
docker compose exec -T postgres psql -U mirrorgate -d candidate_sandbox -qc "TRUNCATE orders" >/dev/null
curl -sf -X POST http://localhost:9003/reset >/dev/null

SINGLE=${SINGLE:-false}
# --build matters: the single-publish flag only exists in a freshly built image.
OUTBOX_SINGLE_PUBLISH="$SINGLE" docker compose up -d --build --force-recreate outbox-relay >/dev/null 2>&1
PUBLISH_MODE=outbox SAMPLING_MODE=full SAMPLING_RATE= SAMPLING_RULES= \
  docker compose up -d --force-recreate gateway >/dev/null 2>&1
for _ in $(seq 1 60); do curl -sf http://localhost:8081/internal/health >/dev/null 2>&1 && break; sleep 1; done
echo "gateway at full sampling, relay single_publish=$SINGLE"

# Confirm the relay is really in the mode this trial claims. A stale image
# ignores the variable, which would record batched numbers under a "before"
# label without anything looking wrong.
mode=""
for _ in $(seq 1 30); do
  mode=$(docker compose logs outbox-relay 2>/dev/null | grep -o 'single_publish=[a-z]*' | tail -1)
  [[ -n "$mode" ]] && break
  sleep 1
done
if [[ "$mode" != "single_publish=$SINGLE" ]]; then
  echo "relay reports '${mode:-nothing}', expected single_publish=$SINGLE" >&2
  exit 1
fi

stopfile="$OUT/.$LABEL.stop"
rm -f "$stopfile"
python experiments/resource_monitor.py "$OUT/$LABEL.resources.json" "$stopfile" &
monitor=$!

# Sample the outbox depth while everything runs. This is the backlog curve.
depth_file="$OUT/.$LABEL.depth"
: > "$depth_file"
(
  start=$(date +%s)
  while [[ ! -f "$stopfile" ]]; do
    echo "$(( $(date +%s) - start )) $(pending) $(stored)" >> "$depth_file"
    sleep 1
  done
) &
sampler=$!

sleep 1
docker compose run --rm -T \
  -e RUN_NAME="$LABEL" -e RATE="$RATE" -e DURATION="$DURATION" -e RESULT_DIR=pr4 \
  k6-rare

load_end=$(date +%s)
echo "load finished, waiting for the queue to clear"
drain_end=$load_end
for _ in $(seq 1 600); do
  if [[ "$(pending)" == "0" ]]; then
    # Let the worker finish what the relay already published.
    last=-1; same=0
    for _ in $(seq 1 60); do
      now=$(stored)
      if [[ "$now" == "$last" ]]; then
        same=$((same + 1)); (( same >= 3 )) && break
      else
        same=0
      fi
      last=$now
      sleep 1
    done
    drain_end=$(date +%s)
    break
  fi
  sleep 1
done

touch "$stopfile"
wait "$monitor" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
rm -f "$stopfile"

curl -s http://localhost:8081/internal/stats > "$OUT/.$LABEL.stats.json"

psql_query -tAc "
  SELECT json_build_object(
    'stored', count(*),
    'lag_p50_ms', round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY lag)::numeric, 1),
    'lag_p95_ms', round(percentile_cont(0.95) WITHIN GROUP (ORDER BY lag)::numeric, 1),
    'lag_max_ms', round(max(lag)::numeric, 1),
    'first_seq_lag_ms', round(min(lag)::numeric, 1)
  ) FROM (
    SELECT EXTRACT(epoch FROM detected_at - received_at) * 1000 AS lag
    FROM request_comparisons
  ) t" | tr -d '\r' > "$OUT/.$LABEL.lag.json"

python experiments/collect_relay.py "$LABEL" "$OUT" "$RATE" "$DURATION" "$(( drain_end - load_end ))"
rm -f "$OUT/.$LABEL.stats.json" "$OUT/.$LABEL.lag.json" "$OUT/.$LABEL.depth" \
      "$OUT/$LABEL.resources.json" "$OUT/$LABEL.k6.json"

docker compose up -d --force-recreate gateway >/dev/null 2>&1
OUTBOX_SINGLE_PUBLISH=false docker compose up -d --build --force-recreate outbox-relay >/dev/null 2>&1
echo "done, gateway and relay back on defaults"
