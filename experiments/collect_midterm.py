"""Merge one midterm run into a single result file.

Three sources have to agree before a number means anything: k6 knows how many
requests the client sent and how long they took, the gateway knows how many of
those it mirrored, and Postgres knows what the worker made of them.

    python experiments/collect_midterm.py risk-t1 results/midterm 100 30s risk
"""

import json
import sys
from pathlib import Path

REGRESSIONS = {
    "R1": "price mismatch on /api/products/12",
    "R2": "candidate 500 on /api/products/8",
    "R3": "slow /api/products?category=audio",
    "R4": "timeout on /api/users/7",
    "R5": "wrong total on POST /api/orders",
}


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def main():
    run, out_dir, rate, duration, mode = sys.argv[1:6]
    out = Path(out_dir)
    rate = float(rate)

    k6 = load(out / f"{run}.k6.json")
    stats = load(out / f"{run}.stats.json")
    db = load(out / f"{run}.db.json")
    sampling = load(out / f"{run}.sampling.json")
    external = load(out / f"{run}.external.json")

    metrics = k6["metrics"]
    duration_ms = metrics["http_req_duration"]["values"]
    total_requests = int(metrics["http_reqs"]["values"]["count"])
    mirrored = stats["gateway"]["mirrored"]
    eligible = stats["gateway"]["eligible"]

    detected = {}
    for rid in REGRESSIONS:
        hits = db[f"{rid.lower()}_hits"]
        first = db[f"{rid.lower()}_first_s"]
        detected[rid] = {
            "detected": hits > 0,
            "hits": hits,
            # Seconds from the first mirrored request to the first time this
            # regression was caught.
            "first_detection_s": round(first, 2) if first is not None else None,
            # The workload is a constant arrival rate, so elapsed time
            # converts to a request count. It is an estimate, not a count of
            # rows, and it is the number the report quotes.
            "requests_until_detection": round(first * rate) if first is not None else None,
        }

    found = sum(1 for d in detected.values() if d["detected"])

    # Side-effect check, run for every trial rather than demonstrated once.
    # One live charge per order the client actually sent, and one sandbox
    # charge per order that was mirrored.
    orders_sent = -(-total_requests // 100)  # one order every 100 requests
    counts = external["counts"]
    sandbox_ok = (
        counts.get("live_payment", 0) == orders_sent
        and counts.get("sandbox_payment", 0) == db["orders_mirrored"]
    )
    detection_times = [d["first_detection_s"] for d in detected.values() if d["first_detection_s"] is not None]

    result = {
        "run": run,
        "strategy": run.rsplit("-t", 1)[0],
        "trial": int(run.rsplit("-t", 1)[1]),
        "mode": mode,
        "workload": {
            "script": "experiments/midterm.js",
            "rate_per_s": rate,
            "duration": duration,
            "total_requests": total_requests,
        },
        "sampling": {
            "mode": stats["sampling"]["mode"],
            "eligible": eligible,
            "mirrored": mirrored,
            "skipped": stats["sampling"]["skipped"],
            "actual_rate_pct": round(100 * mirrored / eligible, 2) if eligible else 0.0,
            "published": stats["gateway"]["published"],
            "publish_failed": stats["gateway"]["publish_failed"],
            "skipped_in_flight": stats["gateway"]["skipped_in_flight"],
            # Only risk mode fills this in.
            "endpoints": sampling.get("endpoints", []),
        },
        "stored": {
            "total": db["stored"],
            "matched": db["matched"],
            "different": db["different"],
            "errors": db["errors"],
            "slow": db["slow"],
            "orders_mirrored": db["orders_mirrored"],
        },
        "detection": {
            "regressions_detected": found,
            "regressions_total": len(REGRESSIONS),
            "detection_rate_pct": round(100 * found / len(REGRESSIONS), 1),
            "slowest_first_detection_s": max(detection_times) if detection_times else None,
            "per_regression": detected,
        },
        "latency_ms": {
            "p50": round(duration_ms["med"], 3),
            "p95": round(duration_ms["p(95)"], 3),
            "p99": round(duration_ms["p(99)"], 3),
            "avg": round(duration_ms["avg"], 3),
            "max": round(duration_ms["max"], 3),
        },
        # Proof that shadow writes stayed in the sandbox for this run.
        "side_effects": {
            "orders_sent_by_client": orders_sent,
            "orders_mirrored": db["orders_mirrored"],
            "calls": counts,
            "isolation_ok": sandbox_ok,
        },
    }

    path = out / f"{run}.json"
    path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")

    print(
        f"{run}: {total_requests} requests, {mirrored} mirrored "
        f"({result['sampling']['actual_rate_pct']}%), "
        f"{found}/{len(REGRESSIONS)} regressions, "
        f"p95 {result['latency_ms']['p95']}ms -> {path}"
    )
    if not sandbox_ok:
        print(
            f"  WARNING: side effects do not line up. {orders_sent} orders sent, "
            f"{db['orders_mirrored']} mirrored, calls {counts}"
        )


if __name__ == "__main__":
    main()
