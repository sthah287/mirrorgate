#!/usr/bin/env bash
# Measure when each regression is actually confirmed, not when the request
# that triggers it arrives.
#
#   ./experiments/confirm-timing.sh
#
# The rare-fault runs recorded the arrival time of the fault-bearing request
# and called it a detection delay, which is wrong: the candidate still has to
# answer, and for R4 that means waiting out the full two second timeout before
# anything can be known. This does one full-sampling run and reports three
# separate numbers per fault:
#
#   request position   the exact request_seq, no clock involved
#   arrival            when the fault-bearing request reached the gateway
#   confirmation       when the worker stored the comparison, which is when
#                      the regression was actually known
#
# Both timestamps come from containers sharing one VM kernel clock, but they
# are different processes, so the gap between them is reported as its own
# figure rather than folded into either.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=results/rare-faults
mkdir -p "$OUT"
RATE=${RATE:-100}
DURATION=${DURATION:-30s}

psql_query() { docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; }

echo "resetting and starting the gateway at full sampling"
psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
docker compose exec -T postgres psql -U mirrorgate -d stable_app -qc "TRUNCATE orders" >/dev/null
docker compose exec -T postgres psql -U mirrorgate -d candidate_sandbox -qc "TRUNCATE orders" >/dev/null
curl -sf -X POST http://localhost:9003/reset >/dev/null

PUBLISH_MODE=outbox SAMPLING_MODE=full SAMPLING_RATE= SAMPLING_RULES= \
  docker compose up -d --force-recreate gateway >/dev/null 2>&1
for _ in $(seq 1 60); do curl -sf http://localhost:8081/internal/health >/dev/null 2>&1 && break; sleep 1; done

docker compose run --rm -T \
  -e RUN_NAME=confirm-timing -e RATE="$RATE" -e DURATION="$DURATION" \
  k6-rare

echo "waiting for the outbox and the topic to drain"
last=-1; same=0
for _ in $(seq 1 120); do
  pending=$(psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r')
  stored=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
  if [[ "$pending" == "0" && "$stored" == "$last" ]]; then
    same=$((same + 1)); (( same >= 3 )) && break
  else
    same=0
  fi
  last=$stored
  sleep 1
done

# Build the query in one piece. An earlier version assembled it in a loop and
# accidentally executed the half-finished fragment.
read -r -d '' CONFIRM_SQL <<'SQL' || true
WITH run AS (SELECT min(received_at) AS started FROM request_comparisons),
fault AS (
  SELECT 'R1' AS id, request_seq, received_at, detected_at, candidate_latency_ms
    FROM request_comparisons WHERE path = '/api/products/12' AND outcome = 'different'
  UNION ALL
  SELECT 'R2', request_seq, received_at, detected_at, candidate_latency_ms
    FROM request_comparisons WHERE path = '/api/products/8' AND outcome = 'error'
  UNION ALL
  SELECT 'R3', request_seq, received_at, detected_at, candidate_latency_ms
    FROM request_comparisons WHERE path = '/api/products' AND query = 'category=audio' AND candidate_slow
  UNION ALL
  SELECT 'R4', request_seq, received_at, detected_at, candidate_latency_ms
    FROM request_comparisons WHERE path = '/api/users/7' AND outcome = 'error'
  UNION ALL
  SELECT 'R5', request_seq, received_at, detected_at, candidate_latency_ms
    FROM request_comparisons WHERE path = '/api/orders' AND method = 'POST' AND outcome = 'different'
),
first_hit AS (
  SELECT DISTINCT ON (id) id, request_seq, received_at, detected_at, candidate_latency_ms
  FROM fault ORDER BY id, request_seq
)
SELECT json_build_object(
  'stored', (SELECT count(*) FROM request_comparisons),
  'run_started_at', (SELECT started FROM run),
  'faults', COALESCE(json_object_agg(id, json_build_object(
      'request_seq', request_seq,
      'arrival_ms', round(EXTRACT(epoch FROM received_at - (SELECT started FROM run))::numeric * 1000, 1),
      'confirmation_ms', round(EXTRACT(epoch FROM detected_at - (SELECT started FROM run))::numeric * 1000, 1),
      'pipeline_ms', round(EXTRACT(epoch FROM detected_at - received_at)::numeric * 1000, 1),
      'candidate_latency_ms', round(candidate_latency_ms::numeric, 1))), '{}'::json)
) FROM first_hit
SQL

psql_query -tAc "$CONFIRM_SQL" | tr -d '' > "$OUT/confirmation-timing.json"
rm -f "$OUT/confirm-timing.k6.json"

python - <<'PY'
import json
from pathlib import Path

data = json.loads(Path("results/rare-faults/confirmation-timing.json").read_text(encoding="utf-8"))
print()
print("one full-sampling run, so every fault is caught on its first occurrence")
print(f"{data['stored']} comparisons stored at {data['rate_per_s']} requests/second\n")
print(f"  {'fault':<6} {'request':>8} {'arrival':>12} {'confirmed':>12} {'pipeline':>11} {'candidate':>11}")
for fault, row in data["faults"].items():
    if not row or not row.get("hits"):
        print(f"  {fault:<6} {'not caught':>8}")
        continue
    print(f"  {fault:<6} {row['request_seq']:>8} {row['arrival_ms']:>10.0f}ms "
          f"{row['confirmation_ms']:>10.0f}ms {row['pipeline_ms']:>9.0f}ms "
          f"{row['candidate_latency_ms']:>9.0f}ms")
print()
print("  arrival   = when the fault-bearing request reached the gateway")
print("  confirmed = when the worker stored the comparison")
print("  pipeline  = confirmed - arrival, which includes waiting for the")
print("              candidate to answer, then the outbox, relay and worker")
PY

docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo
echo "wrote results/rare-faults/confirmation-timing.json, gateway back on defaults"
