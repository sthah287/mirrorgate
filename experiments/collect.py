"""Merge one experiment run into a single result file.

Three sources have to line up for a run to mean anything: what k6 measured on
the client side, what the gateway counted, and what actually ended up in
Postgres. Keeping them in one file per strategy makes the summary table a
straight read rather than a join.

    python experiments/collect.py random-25 results/progress-report-2
"""

import json
import sys
from pathlib import Path


def main():
    name, out_dir = sys.argv[1], Path(sys.argv[2])

    k6 = json.loads((out_dir / f"{name}.k6.json").read_text())
    stats = json.loads((out_dir / f"{name}.stats.json").read_text())
    db = json.loads((out_dir / f"{name}.db.json").read_text())

    duration = k6["metrics"]["http_req_duration"]["values"]
    reqs = k6["metrics"]["http_reqs"]["values"]
    gateway = stats["gateway"]
    sampling = stats["sampling"]

    eligible = sampling["mirrored"] + sampling["skipped"]
    known = {
        "price (product 12)": db["price_regression"],
        "status 500 (product 8)": db["status_regression"],
        "slow response (audio list)": db["slow_regression"],
        "timeout (user 7)": db["timeout_regression"],
    }

    result = {
        "strategy": name,
        "sampling": {
            "mode": sampling["mode"],
            "target_rate": sampling["rate"],
            "rules": sampling.get("rules"),
        },
        "traffic": {
            "total_requests": int(reqs["count"]),
            # k6 measures its own run window, which is a little shorter than the
            # configured duration. Recording it makes the rate below reproducible
            # instead of a number that cannot be checked.
            "test_duration_s": round(k6["state"]["testRunDurationMs"] / 1000, 1),
            "throughput_rps": round(reqs["rate"], 1),
            "failed_rate": k6["metrics"]["http_req_failed"]["values"]["rate"],
            # Non-zero means k6 could not keep up with the arrival rate, which
            # would make a latency comparison between runs unfair.
            "dropped_iterations": int(
                k6["metrics"].get("dropped_iterations", {}).get("values", {}).get("count", 0)
            ),
        },
        "mirroring": {
            "eligible": eligible,
            "mirrored": sampling["mirrored"],
            "skipped_by_sampling": sampling["skipped"],
            "skipped_in_flight": gateway["skipped_in_flight"],
            "actual_rate": round(sampling["mirrored"] / eligible, 4) if eligible else 0,
            "events_published": gateway["published"],
            "publish_failures": gateway["publish_failed"],
            "comparisons_stored": db["stored"],
        },
        "client_latency_ms": {
            "avg": round(duration["avg"], 2),
            "p50": round(duration["med"], 2),
            "p95": round(duration["p(95)"], 2),
            "p99": round(duration["p(99)"], 2),
            "max": round(duration["max"], 2),
        },
        "outcomes": {
            "match": db["matched"],
            "different": db["different"],
            "error": db["errors"],
            "slow_flagged": db["slow"],
        },
        "regressions": {
            "detected": {k: v for k, v in known.items()},
            "missed": [k for k, v in known.items() if v == 0],
            "seconds_to_first_detection": {
                "price (product 12)": db["first_price_after_s"],
                "status 500 (product 8)": db["first_status_after_s"],
                "slow response (audio list)": db["first_slow_after_s"],
                "timeout (user 7)": db["first_timeout_after_s"],
            },
        },
    }

    path = out_dir / f"{name}.json"
    path.write_text(json.dumps(result, indent=2) + "\n")

    m = result["mirroring"]
    print(
        f"  mirrored {m['mirrored']}/{m['eligible']} "
        f"({m['actual_rate'] * 100:.1f}%), stored {m['comparisons_stored']}, "
        f"missed {len(result['regressions']['missed'])} of 4 regressions"
    )


if __name__ == "__main__":
    main()
