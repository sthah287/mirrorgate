#!/usr/bin/env bash
# Record a sample of read-only traffic, replay it at the candidate, and show
# that the parts that are supposed to be impossible are impossible.
#
#   ./scripts/replay-demo.sh
#
# Recording and the replay trigger are both off in the normal stack. This
# turns them on with a token generated for this run, exercises them, and puts
# the gateway back the way it was.
#
# What it demonstrates, in order:
#   recording keeps GETs only, with no credentials in the stored headers
#   the trigger refuses to run without the token, and with the wrong one
#   a replay reaches the candidate and never the stable service
#   the live payment and email endpoints are never called
set -euo pipefail

cd "$(dirname "$0")/.."
GATEWAY=${GATEWAY:-http://localhost:8080}
ADMIN=${ADMIN:-http://localhost:8081}
REQUESTS=${REQUESTS:-30}

TOKEN=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
psql_query() { docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; }

restore() {
  docker compose up -d --force-recreate gateway >/dev/null 2>&1
  for _ in $(seq 1 60); do
    curl -sf "$ADMIN/internal/health" >/dev/null 2>&1 && break
    sleep 1
  done
  echo
  echo "gateway back on the defaults, recording off"
}
trap restore EXIT

echo "starting the gateway with recording on and a one-off replay token"
REPLAY_RECORD=true REPLAY_RECORD_RATE=1 REPLAY_MAX_RECORDS=200 REPLAY_RETENTION=1h \
  REPLAY_TOKEN="$TOKEN" REPLAY_MAX_BATCH=25 \
  docker compose up -d --build --force-recreate gateway >/dev/null 2>&1
for _ in $(seq 1 60); do
  curl -sf "$ADMIN/internal/health" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf -X POST http://localhost:9003/reset >/dev/null
# Cleared so the counts below are this run's. Recordings last an hour, so
# without this a second run reports the first run's rows as its own.
psql_query -qc "TRUNCATE recorded_requests, replay_results" >/dev/null

echo
echo "=================================================================="
echo " 1. recording reads, and only reads"
echo "=================================================================="
for i in $(seq 1 "$REQUESTS"); do
  curl -s -o /dev/null -H "Authorization: Bearer not-a-real-token" \
    -H "Cookie: session=not-a-real-session" \
    "$GATEWAY/api/products/$(( (i % 9) + 1 ))"
done
# A write, to show it is not recorded.
curl -s -o /dev/null -X POST -H 'Content-Type: application/json' \
  -d '{"product_id":1,"quantity":1}' "$GATEWAY/api/orders"
sleep 2

psql_query -tAc "
  SELECT '  recorded ' || count(*) || ' requests, methods: ' ||
         string_agg(DISTINCT method, ',')
  FROM recorded_requests" | tr -d '\r'
echo "  headers kept on the most recent recording:"
psql_query -tAc "
  SELECT '    ' || coalesce(string_agg(key, ', '), '(none)')
  FROM (SELECT request_id, recorded_at FROM recorded_requests
        ORDER BY recorded_at DESC LIMIT 1) r
  JOIN LATERAL jsonb_object_keys(
    (SELECT headers FROM recorded_requests WHERE request_id = r.request_id)) AS key ON true" \
  | tr -d '\r'
echo "  rows whose stored headers mention a credential:"
psql_query -tAc "
  SELECT '    ' || count(*)
  FROM recorded_requests
  WHERE headers::text ILIKE '%authorization%'
     OR headers::text ILIKE '%cookie%'
     OR headers::text ILIKE '%token%'" | tr -d '\r'

echo
echo "=================================================================="
echo " 2. the trigger is closed without the token"
echo "=================================================================="
printf '  no token:    HTTP %s\n'  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$ADMIN/internal/replay")"
printf '  wrong token: HTTP %s\n'  "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H 'X-Replay-Token: guessing' "$ADMIN/internal/replay")"
echo "  replays recorded so far: $(psql_query -tAc 'SELECT count(*) FROM replay_results' | tr -d '\r')"

echo
echo "=================================================================="
echo " 3. a replay, with the token"
echo "=================================================================="
stable_orders() {
  docker compose exec -T postgres psql -U mirrorgate -d stable_app -tAc "SELECT count(*) FROM orders" | tr -d '\r'
}
before_stable=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
before_orders=$(stable_orders)
curl -s -X POST -H "X-Replay-Token: $TOKEN" "$ADMIN/internal/replay?limit=10" \
  | python -c "import sys,json;s=json.load(sys.stdin);print('  batch',s['batch_id'],'->',s['replayed'],'replayed,',s['failed'],'failed, statuses',s['status_counts'],'in',round(s['duration_ms'],1),'ms')"

echo "  stored outcomes:"
curl -s "$ADMIN/internal/replay/results?limit=5" \
  | python -c "
import sys, json
for r in json.load(sys.stdin):
    print(f\"    {r['path']:22s} status={r['status']} {r['latency_ms']:.1f}ms\")"

echo
echo "=================================================================="
echo " 4. what the replay did not touch"
echo "=================================================================="
after_stable=$(psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r')
echo "  comparisons before the replay: $before_stable, after: $after_stable"
echo "  (a replay is not a comparison, so this number should not move)"
curl -s http://localhost:9003/calls | python -c "
import sys, json
calls = json.load(sys.stdin)
live = [c for c in calls.get('calls', calls) if '/sandbox/' not in c.get('path', '')]
print(f\"  live payment and email calls during the whole demo: {len(live)}\")"
echo "  rows in the stable application's orders table before: $before_orders, after: $(stable_orders)"
echo "  (replays are GET only, so this number cannot move either)"
