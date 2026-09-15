#!/usr/bin/env bash
# Generates a steady mix of traffic so the dashboard has more than a handful
# of rows. Most requests should match; the regressions show up once per round.
#
#   ./scripts/traffic.sh      5 rounds
#   ./scripts/traffic.sh 20   20 rounds
set -euo pipefail

GATEWAY=${GATEWAY:-http://localhost:8080}
rounds=${1:-5}

paths=(/api/products "/api/products?category=audio" "/api/products?category=input")
for id in $(seq 1 12); do
  paths+=("/api/products/$id")
done
for id in 1 2 3 7; do
  paths+=("/api/users/$id")
done

for round in $(seq 1 "$rounds"); do
  for path in "${paths[@]}"; do
    curl -s -o /dev/null "$GATEWAY$path"
  done
  echo "round $round: sent ${#paths[@]} requests"
done
