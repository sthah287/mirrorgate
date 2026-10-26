#!/bin/sh
# Runs the test suite for every Go module in the repo. Used by the `tests`
# service in docker-compose so this works without Go installed locally.
set -e
cd "$(dirname "$0")/.."

for module in shared gateway comparison-worker outbox-relay tests; do
  echo
  echo "===== $module ====="
  (cd "$module" && go test -count=1 "$@" ./...)
done
