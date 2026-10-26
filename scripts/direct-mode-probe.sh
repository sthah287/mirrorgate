#!/usr/bin/env bash
# Why does direct publishing sometimes lose nothing during an outage?
#
# The gateway's publish call fails for every request while the broker is down,
# yet afterwards somewhere between none and all of those events turn up. This
# tells the two candidate explanations apart:
#
#   A. the Kafka client's own retry loop delivers them once the broker is back,
#      with no help needed
#   B. the messages sit in an internal batch and only go out when later traffic
#      forces a flush
#
# Run A first: recover the broker and send nothing. Then run B: send traffic
# and see whether the same events appear.
#
#   ./scripts/direct-mode-probe.sh
set -euo pipefail

cd "$(dirname "$0")/.."
GATEWAY=${GATEWAY:-http://localhost:8080}
ADMIN=${ADMIN:-http://localhost:8081}
REQUESTS=${REQUESTS:-10}

psql_query() { docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"; }

stored_of() {
  local ids=("$@") list=""
  for id in "${ids[@]}"; do list+="'$id',"; done
  psql_query -tAc "SELECT count(*) FROM request_comparisons WHERE request_id IN (${list%,})" | tr -d '\r'
}

send_requests() {
  local n=$1 ids=()
  for _ in $(seq 1 "$n"); do
    local id
    id=$(curl -sf -D - -o /dev/null "$GATEWAY/api/products/12" 2>/dev/null \
      | tr -d '\r' | awk 'tolower($1) == "x-request-id:" {print $2}')
    [[ -n "$id" ]] && ids+=("$id")
  done
  printf '%s\n' "${ids[@]}"
}

wait_for_broker() {
  for _ in $(seq 1 60); do
    docker compose exec -T redpanda rpk cluster health 2>/dev/null | grep -q 'Healthy:.*true' && return 0
    sleep 2
  done
}

echo "putting the gateway in direct mode"
PUBLISH_MODE=direct docker compose up -d --force-recreate gateway >/dev/null 2>&1
for _ in $(seq 1 60); do curl -sf "$ADMIN/internal/health" >/dev/null 2>&1 && break; sleep 1; done
psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
psql_query -qc "TRUNCATE comparison_outbox" >/dev/null

docker compose stop redpanda >/dev/null 2>&1
echo "broker stopped, sending $REQUESTS requests"
mapfile -t ids < <(send_requests "$REQUESTS")
sleep 8
failed=$(curl -s "$ADMIN/internal/stats" | python -c "import sys,json;print(json.load(sys.stdin)['gateway']['publish_failed'])")
echo "  the gateway's publish call failed for $failed of ${#ids[@]}"

docker compose start redpanda >/dev/null 2>&1
wait_for_broker
echo "broker is back"

echo
echo "A. waiting 45s with no further traffic at all"
for i in 15 30 45; do
  sleep 15
  echo "  after ${i}s: $(stored_of "${ids[@]}") of ${#ids[@]} stored"
done
quiet=$(stored_of "${ids[@]}")

echo
echo "B. now sending 5 unrelated requests to force a flush"
send_requests 5 >/dev/null
sleep 10
after=$(stored_of "${ids[@]}")
echo "  after traffic: $after of ${#ids[@]} stored"

echo
echo "=============================================================="
echo " recovered with no traffic : $quiet of ${#ids[@]}"
echo " recovered after traffic   : $after of ${#ids[@]}"
if (( after > quiet )); then
  echo " -> the events were held in the client and only went out when later"
  echo "    traffic flushed the batch, which is why the number varies"
elif (( quiet == ${#ids[@]} )); then
  echo " -> the client's own retry delivered them without any help"
else
  echo " -> neither: $(( ${#ids[@]} - after )) events were genuinely dropped"
fi
echo "=============================================================="

PUBLISH_MODE=outbox docker compose up -d --force-recreate gateway >/dev/null 2>&1
echo "gateway put back in outbox mode"
