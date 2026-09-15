#!/usr/bin/env bash
# Sends one request for each scenario MirrorGate should detect, then prints
# what got stored. Run from anywhere while `docker compose up` is running.
#
#   ./scripts/demo.sh          keep existing comparisons
#   ./scripts/demo.sh --reset  clear the table first
set -euo pipefail

cd "$(dirname "$0")/.."

GATEWAY=${GATEWAY:-http://localhost:8080}

psql_query() {
  docker compose exec -T postgres psql -U mirrorgate -d mirrorgate "$@"
}

if [[ "${1:-}" == "--reset" ]]; then
  psql_query -qc "TRUNCATE request_comparisons RESTART IDENTITY"
  echo "cleared old comparisons"
fi

before=$(psql_query -tAc "SELECT count(*) FROM request_comparisons")

send() {
  printf '\n== %s\n' "$1"
  printf 'GET %s\n' "$2"
  curl -s -w '\n-> client got %{http_code} in %{time_total}s\n' "$GATEWAY$2"
}

send "1. normal request, should match"               "/api/products/1"
send "2. candidate returns a different price"        "/api/products/12"
send "3. candidate returns HTTP 500"                 "/api/products/8"
send "4. candidate is ~400ms slower"                 "/api/products?category=audio"
send "5. candidate hangs past the 2s shadow timeout" "/api/users/7"
send "6. same data, different JSON formatting"       "/api/users/3"
send "7. 404 from both versions, should match"       "/api/products/99"

# The client responses above come back right away, but the comparisons are
# saved in the background, so wait until all 7 rows show up.
expected=$((before + 7))
printf '\nwaiting for shadow comparisons'
for _ in $(seq 1 20); do
  count=$(psql_query -tAc "SELECT count(*) FROM request_comparisons")
  if (( count >= expected )); then
    break
  fi
  printf '.'
  sleep 0.5
done
echo

psql_query -c "
  SELECT method || ' ' || path || CASE WHEN query <> '' THEN '?' || query ELSE '' END AS request,
         stable_status AS stable,
         COALESCE(candidate_status::text, '-') AS candidate,
         round(stable_latency_ms::numeric, 1) AS stable_ms,
         round(candidate_latency_ms::numeric, 1) AS candidate_ms,
         outcome,
         candidate_slow AS slow,
         COALESCE(candidate_error, array_to_string(differences, '; ')) AS details
  FROM request_comparisons
  ORDER BY received_at DESC
  LIMIT 7"

echo "dashboard: http://localhost:3000"
