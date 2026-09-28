#!/usr/bin/env bash
# Checks what happens to MirrorGate when parts of the new pipeline go down.
# Two scenarios, both against the running stack:
#
#   1. the comparison worker stops    -> events wait in Kafka, then get processed
#   2. the broker stops               -> the client is fine, but MirrorGate
#                                        cannot promise the comparison survives
#
#   ./scripts/failure-test.sh
set -euo pipefail

cd "$(dirname "$0")/.."
GATEWAY=${GATEWAY:-http://localhost:8080}
ADMIN=${ADMIN:-http://localhost:8081}
REQUESTS=10

psql_query() {
  docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"
}

rows() {
  psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r'
}

published() {
  curl -s "$ADMIN/internal/stats" | python -c "import sys,json;print(json.load(sys.stdin)['gateway']['published'])"
}

publish_failed() {
  curl -s "$ADMIN/internal/stats" | python -c "import sys,json;print(json.load(sys.stdin)['gateway']['publish_failed'])"
}

# Sends traffic and fails loudly if any client request didn't get the stable
# response. That is the property that has to hold in every scenario.
# The request ids it sent are left in SENT_IDS so a caller can check later
# which of those requests actually produced a stored comparison.
SENT_IDS=""
send_traffic() {
  local label=$1 count=$2 ok=0 headers id
  SENT_IDS=""
  for _ in $(seq 1 "$count"); do
    headers=$(curl -sf -D - -o /tmp/mirrorgate-body "$GATEWAY/api/products/12") || true
    if grep -q '49.99' /tmp/mirrorgate-body 2>/dev/null; then
      ok=$((ok + 1))
    fi
    id=$(printf '%s' "$headers" | grep -i '^X-Request-Id' | tr -d '
' | awk '{print $2}')
    SENT_IDS="$SENT_IDS $id"
  done
  echo "  $label: $ok/$count clients got the stable response"
  if (( ok != count )); then
    echo "  FAIL: user traffic was affected" >&2
    exit 1
  fi
}

# How many of the given request ids ended up stored as comparisons.
stored_count() {
  local list quoted=""
  for id in $1; do
    quoted="$quoted,'$id'"
  done
  list=${quoted#,}
  psql_query -tAc "SELECT count(*) FROM request_comparisons WHERE request_id IN ($list)" | tr -d '
'
}

wait_for_rows() {
  local want=$1
  for _ in $(seq 1 40); do
    if (( $(rows) >= want )); then
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "=================================================================="
echo " Scenario 1: comparison worker is down"
echo "=================================================================="
psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
docker compose stop comparison-worker >/dev/null 2>&1
echo "worker stopped"

before_published=$(published)
send_traffic "worker down" "$REQUESTS"
sleep 3

after_published=$(published)
echo "  events published while the worker was down: $((after_published - before_published))"
echo "  comparisons stored so far: $(rows)  (expected 0)"
if (( $(rows) != 0 )); then
  echo "  FAIL: something stored comparisons with no worker running" >&2
  exit 1
fi

echo "  queued in the topic:"
docker compose exec -T redpanda rpk topic describe mirrorgate.comparisons -p 2>/dev/null \
  | awk '/PARTITION|^ *0/ {print "    " $0}'

docker compose start comparison-worker >/dev/null 2>&1
echo "worker restarted"
if wait_for_rows "$REQUESTS"; then
  echo "  PASS: worker caught up, $(rows) comparisons stored"
else
  echo "  FAIL: queued events were not processed after restart (stored $(rows))" >&2
  exit 1
fi

echo
echo "=================================================================="
echo " Scenario 2: broker is down"
echo "=================================================================="
stored_before=$(rows)
failed_before=$(publish_failed)

docker compose stop redpanda >/dev/null 2>&1
echo "redpanda stopped"

send_traffic "broker down" "$REQUESTS"
sleep 8

failed_after=$(publish_failed)
outage_ids=$SENT_IDS
echo "  publish failures counted: $((failed_after - failed_before))"
echo "  comparisons stored: $(rows)  (was $stored_before)"

docker compose start redpanda >/dev/null 2>&1
echo "redpanda restarted, waiting for the cluster"
for _ in $(seq 1 40); do
  if docker compose exec -T redpanda rpk cluster health 2>/dev/null | grep -q 'Healthy:.*true'; then
    break
  fi
  sleep 2
done

# There is no outbox or retry queue, so a publish that fails is not replayed
# by MirrorGate. What happens to those events is up to the Kafka client's
# internal buffer, and it is not reliable: in some runs the messages are
# flushed once traffic resumes, in others they are dropped. Either way the
# gateway cannot promise the comparison was recorded, which is the limitation
# written up in the README.
sleep 5
stored_after_recovery=$(rows)
landed=$(stored_count "$outage_ids")
echo "  comparisons stored after recovery: $stored_after_recovery"
echo "  of the $REQUESTS requests sent during the outage, $landed were stored"
echo "  (publish returned an error for $((failed_after - failed_before)) of them,"
echo "   so the publish_failed counter is an upper bound, not a loss count)"

echo "  sending traffic again to confirm the pipeline recovered"
target=$((stored_after_recovery + REQUESTS))
send_traffic "after recovery" "$REQUESTS"
if wait_for_rows "$target"; then
  echo "  PASS: publishing works again, $(rows) comparisons stored"
else
  echo "  FAIL: pipeline did not recover (stored $(rows), wanted $target)" >&2
  exit 1
fi

# Worth re-checking: the Kafka client can hold an unacknowledged batch and
# flush it on the next successful write, so events that looked lost a moment
# ago sometimes appear once traffic resumes. That is luck, not a guarantee.
landed_later=$(stored_count "$outage_ids")
echo "  re-checking the outage requests: $landed_later of $REQUESTS are stored now"
if (( landed_later > landed )); then
  echo "  -> they were buffered in the Kafka client and flushed by the new traffic"
elif (( landed_later == 0 )); then
  echo "  -> they are gone; nothing replays a failed publish yet"
fi

echo
echo "both scenarios finished"
