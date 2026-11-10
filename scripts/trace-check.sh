#!/usr/bin/env bash
# Does one trace still follow a request all the way through, now that the
# relay publishes a whole batch in one call?
#
#   ./scripts/trace-check.sh [requests]
#
# Sends a burst, so several comparison events are claimed and published
# together, then asks Jaeger for each request id. A request passes when its
# gateway, outbox-relay and comparison-worker spans are in one trace, and the
# burst passes when every request got its own trace rather than all of them
# being folded into whichever trace the relay happened to be in.
set -euo pipefail

cd "$(dirname "$0")/.."
GATEWAY=${GATEWAY:-http://localhost:8080}
JAEGER=${JAEGER:-http://localhost:16686}
REQUESTS=${1:-20}

ids=()
for _ in $(seq 1 "$REQUESTS"); do
  id=$(curl -sf -D - -o /dev/null "$GATEWAY/api/products/12" 2>/dev/null \
    | tr -d '\r' | awk 'tolower($1) == "x-request-id:" {print $2}')
  [[ -n "$id" ]] && ids+=("$id")
done
if [[ ${#ids[@]} -ne $REQUESTS ]]; then
  echo "only ${#ids[@]} of $REQUESTS requests came back with a request id" >&2
  exit 1
fi
echo "sent ${#ids[@]} requests, waiting for the spans to land"
sleep 8

# Ids go on the command line: a temp file would need a path that means the
# same thing to Git Bash and to Windows Python, and these are short.
python - "$JAEGER" "${ids[@]}" <<'PY'
import json
import sys
import urllib.parse
import urllib.request

jaeger = sys.argv[1]
want = {"gateway", "outbox-relay", "comparison-worker"}
ids = sys.argv[2:]

complete, traces, broken = 0, set(), []
for request_id in ids:
    query = urllib.parse.urlencode({
        "service": "gateway",
        "lookback": "1h",
        "limit": "10",
        "tags": json.dumps({"mirrorgate.request_id": request_id}),
    })
    with urllib.request.urlopen(f"{jaeger}/api/traces?{query}") as response:
        found = json.load(response)["data"]
    services = set()
    for trace in found:
        traces.add(trace["traceID"])
        services |= {p["serviceName"] for p in trace["processes"].values()}
    if want <= services and len(found) == 1:
        complete += 1
    else:
        broken.append((request_id, sorted(services), len(found)))

print(f"  requests with one trace covering gateway, relay and worker: {complete}/{len(ids)}")
print(f"  distinct trace ids: {len(traces)} (want {len(ids)}, one per request)")
for request_id, services, count in broken[:5]:
    print(f"  incomplete: {request_id} -> {count} traces, services {services}")
if ids and complete == len(ids) and len(traces) == len(ids):
    print("  PASS")
else:
    print("  FAIL: tracing did not survive the batch")
    sys.exit(1)
PY
