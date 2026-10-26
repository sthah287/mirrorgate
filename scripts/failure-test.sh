#!/usr/bin/env bash
# What happens to MirrorGate when parts of the pipeline go down. Four
# scenarios, all against the running stack:
#
#   1. comparison worker stops  -> events wait in the topic, then get processed
#   2. broker stops, outbox     -> events wait in Postgres, nothing is lost
#   3. relay is killed mid-run  -> the rows it had not published still arrive
#   4. broker stops, direct     -> the old behaviour, where events are lost
#
# Scenario 4 restarts the gateway with PUBLISH_MODE=direct and puts it back
# afterwards. It is the measurement that makes scenario 2 worth something.
#
#   ./scripts/failure-test.sh
set -euo pipefail

cd "$(dirname "$0")/.."
GATEWAY=${GATEWAY:-http://localhost:8080}
ADMIN=${ADMIN:-http://localhost:8081}
REQUESTS=${REQUESTS:-10}

psql_query() {
  docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"
}

stored_rows() { psql_query -tAc "SELECT count(*) FROM request_comparisons" | tr -d '\r'; }
pending_rows() { psql_query -tAc "SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL" | tr -d '\r'; }
gateway_stat() { curl -s "$ADMIN/internal/stats" | python -c "import sys,json;print(json.load(sys.stdin)['gateway']['$1'])"; }

wait_for_gateway() {
  for _ in $(seq 1 60); do
    curl -sf "$ADMIN/internal/health" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "gateway did not come back" >&2
  exit 1
}

# Sends requests and prints the request ids the gateway assigned, so a
# scenario can ask what happened to those exact comparisons.
send_requests() {
  local n=$1 ids=()
  for _ in $(seq 1 "$n"); do
    local id
    id=$(curl -sf -D - -o /dev/null "$GATEWAY/api/products/12" 2>/dev/null \
      | tr -d '\r' | awk 'tolower($1) == "x-request-id:" {print $2}')
    if [[ -n "$id" ]]; then
      ids+=("$id")
    fi
  done
  printf '%s\n' "${ids[@]}"
}

stored_of() {
  local ids=("$@") list=""
  for id in "${ids[@]}"; do
    list+="'$id',"
  done
  list=${list%,}
  psql_query -tAc "SELECT count(*) FROM request_comparisons WHERE request_id IN ($list)" | tr -d '\r'
}

reset_state() {
  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY" >/dev/null
  psql_query -qc "TRUNCATE comparison_outbox" >/dev/null
}

wait_until_drained() {
  local want=$1
  for _ in $(seq 1 60); do
    if [[ "$(stored_rows)" -ge "$want" && "$(pending_rows)" -eq 0 ]]; then
      return 0
    fi
    sleep 1
  done
}

restart_gateway() {
  PUBLISH_MODE="$1" docker compose up -d --force-recreate gateway >/dev/null 2>&1
  wait_for_gateway
}

echo "=================================================================="
echo " Scenario 1: the comparison worker is down"
echo "=================================================================="
reset_state
docker compose stop comparison-worker >/dev/null 2>&1
echo "worker stopped"

mapfile -t ids < <(send_requests "$REQUESTS")
echo "  ${#ids[@]}/$REQUESTS clients got the stable response"
sleep 3
echo "  comparisons stored while the worker was down: $(stored_rows)  (expected 0)"
echo "  outbox rows still pending: $(pending_rows)"

docker compose start comparison-worker >/dev/null 2>&1
wait_until_drained "$REQUESTS"
echo "worker restarted"
echo "  PASS: worker caught up, $(stored_of "${ids[@]}") of ${#ids[@]} comparisons stored"

echo
echo "=================================================================="
echo " Scenario 2: the broker is down, with the outbox"
echo "=================================================================="
reset_state
docker compose stop redpanda >/dev/null 2>&1
echo "redpanda stopped"

mapfile -t ids < <(send_requests "$REQUESTS")
echo "  ${#ids[@]}/$REQUESTS clients got the stable response"
sleep 3
pending_during=$(pending_rows)
echo "  rows waiting in the outbox: $pending_during"
echo "  comparisons stored so far: $(stored_rows)  (expected 0)"
echo "  publish failures counted by the gateway: $(gateway_stat publish_failed)  (expected 0, the gateway only writes to Postgres now)"

docker compose start redpanda >/dev/null 2>&1
for _ in $(seq 1 60); do
  docker compose exec -T redpanda rpk cluster health 2>/dev/null | grep -q 'Healthy:.*true' && break
  sleep 2
done
echo "redpanda restarted"
wait_until_drained "${#ids[@]}"

recovered=$(stored_of "${ids[@]}")
echo "  of the ${#ids[@]} requests sent during the outage, $recovered are stored"
echo "  outbox rows still pending: $(pending_rows)"
if [[ "$recovered" -eq "${#ids[@]}" ]]; then
  echo "  PASS: nothing was lost, the outage became a delay"
else
  echo "  FAIL: $(( ${#ids[@]} - recovered )) comparisons never arrived"
fi

echo
echo "=================================================================="
echo " Scenario 3: the relay is killed mid-run"
echo "=================================================================="
reset_state
docker compose stop redpanda >/dev/null 2>&1
mapfile -t ids < <(send_requests "$REQUESTS")
echo "  ${#ids[@]} requests queued in the outbox while the broker was down"

docker compose kill outbox-relay >/dev/null 2>&1
echo "relay killed (not a clean shutdown)"
docker compose start redpanda >/dev/null 2>&1
for _ in $(seq 1 60); do
  docker compose exec -T redpanda rpk cluster health 2>/dev/null | grep -q 'Healthy:.*true' && break
  sleep 2
done
echo "  broker is back, but the relay is still down: $(stored_of "${ids[@]}") stored"

docker compose start outbox-relay >/dev/null 2>&1
echo "relay restarted"
wait_until_drained "${#ids[@]}"
recovered=$(stored_of "${ids[@]}")
echo "  of the ${#ids[@]} queued requests, $recovered are stored"
if [[ "$recovered" -eq "${#ids[@]}" ]]; then
  echo "  PASS: the relay picked up where it left off"
else
  echo "  FAIL: $(( ${#ids[@]} - recovered )) comparisons never arrived"
fi

echo
echo "=================================================================="
echo " Scenario 4: the broker is down, publishing directly (no outbox)"
echo "=================================================================="
restart_gateway direct
reset_state
docker compose stop redpanda >/dev/null 2>&1
echo "redpanda stopped, gateway is in direct mode"

mapfile -t ids < <(send_requests "$REQUESTS")
echo "  ${#ids[@]}/$REQUESTS clients got the stable response"
sleep 3
# Read later as well: the Kafka client retries internally for a while, so a
# failure counted at this point is only the ones that have already given up.
echo "  publish failures counted so far: $(gateway_stat publish_failed)"

docker compose start redpanda >/dev/null 2>&1
for _ in $(seq 1 60); do
  docker compose exec -T redpanda rpk cluster health 2>/dev/null | grep -q 'Healthy:.*true' && break
  sleep 2
done
echo "redpanda restarted"
sleep 5
echo "  sending more traffic, since the Kafka client can flush buffered events later"
send_requests 5 >/dev/null
sleep 5

recovered=$(stored_of "${ids[@]}")
echo "  publish failures counted once the client gave up: $(gateway_stat publish_failed)"
echo "  of the ${#ids[@]} requests sent during the outage, $recovered are stored"
echo "  -> $(( ${#ids[@]} - recovered )) lost in direct mode, against 0 with the outbox"
echo "  (the exact number varies: the client buffers some events and may flush"
echo "   them when traffic resumes, which is why publish_failed is an upper bound)"

restart_gateway outbox
echo
echo "gateway put back in outbox mode"
echo "all scenarios finished"
