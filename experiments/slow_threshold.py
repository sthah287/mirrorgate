"""Check the 200ms slow threshold against measured data.

    python experiments/slow_threshold.py results/overhead

COMPARE_SLOW_THRESHOLD_MS decides when the candidate counts as significantly
slower. The number was picked by hand, so this measures what the difference
actually looks like on endpoints that are not supposed to be slow, and what it
looks like on the one that is.

Run it straight after a full-mirroring run, while the comparisons from that
run are still in the database.
"""

import json
import subprocess
import sys
from pathlib import Path

QUERY = """
SELECT json_build_object(
  'clean', (
    SELECT json_build_object(
      'samples', count(*),
      'p50', round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY d)::numeric, 3),
      'p95', round(percentile_cont(0.95) WITHIN GROUP (ORDER BY d)::numeric, 3),
      'p99', round(percentile_cont(0.99) WITHIN GROUP (ORDER BY d)::numeric, 3),
      'p999', round(percentile_cont(0.999) WITHIN GROUP (ORDER BY d)::numeric, 3),
      'max', round(max(d)::numeric, 3)
    )
    FROM (
      SELECT candidate_latency_ms - stable_latency_ms AS d
      FROM request_comparisons
      WHERE candidate_error IS NULL
        AND NOT (path = '/api/products' AND query = 'category=audio')
        AND path NOT IN ('/api/products/12', '/api/products/8', '/api/users/7', '/api/orders')
    ) clean
  ),
  'slow_endpoint', (
    SELECT json_build_object(
      'samples', count(*),
      'p50', round(percentile_cont(0.5) WITHIN GROUP (ORDER BY d)::numeric, 3),
      'min', round(min(d)::numeric, 3),
      'max', round(max(d)::numeric, 3)
    )
    FROM (
      SELECT candidate_latency_ms - stable_latency_ms AS d
      FROM request_comparisons
      WHERE path = '/api/products' AND query = 'category=audio' AND candidate_error IS NULL
    ) slow
  )
)
"""


def main():
    out = Path(sys.argv[1] if len(sys.argv) > 1 else "results/overhead")
    out.mkdir(parents=True, exist_ok=True)

    result = subprocess.run(
        ["docker", "compose", "exec", "-T", "postgres",
         "psql", "-U", "mirrorgate", "-d", "mirrorgate", "-tAc", QUERY],
        capture_output=True, text=True, timeout=60,
    )
    if result.returncode != 0:
        print(f"query failed: {result.stderr.strip()}")
        return

    data = json.loads(result.stdout.strip())
    clean = data.get("clean") or {}
    slow = data.get("slow_endpoint") or {}

    threshold = 200.0
    headroom = None
    if clean.get("max") is not None:
        headroom = round(threshold / float(clean["max"]), 1) if float(clean["max"]) > 0 else None

    payload = {
        "threshold_ms": threshold,
        "clean_endpoints": clean,
        "slow_endpoint_r3": slow,
        "headroom_over_clean_max": headroom,
        "note": "clean endpoints are every mirrored comparison except the four faulty paths; "
                "the difference is candidate_latency_ms - stable_latency_ms on the same request",
    }
    (out / "slow-threshold.json").write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")

    print(f"clean endpoints: {clean.get('samples')} samples, "
          f"p99 {clean.get('p99')}ms, max {clean.get('max')}ms")
    print(f"R3 slow endpoint: {slow.get('samples')} samples, "
          f"p50 {slow.get('p50')}ms, min {slow.get('min')}ms")
    print(f"threshold {threshold}ms sits {headroom}x above the worst clean sample")


if __name__ == "__main__":
    main()
