"""Merge one overhead run (mirroring off, or mirroring everything).

    python experiments/collect_overhead.py full-t1 results/overhead 500 20s
"""

import json
import sys
from pathlib import Path


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def main():
    run, out_dir, rate, duration = sys.argv[1:5]
    out = Path(out_dir)

    k6 = load(out / f"{run}.k6.json")
    stats = load(out / f"{run}.stats.json")
    resources_path = out / f"{run}.resources.json"
    resources = load(resources_path) if resources_path.exists() else {}

    values = k6["metrics"]["http_req_duration"]["values"]
    gateway = stats["gateway"]
    total = int(k6["metrics"]["http_reqs"]["values"]["count"])

    result = {
        "run": run,
        "arm": run.rsplit("-t", 1)[0],
        "trial": int(run.rsplit("-t", 1)[1]),
        "rate_per_s": float(rate),
        "duration": duration,
        "total_requests": total,
        "mirrored": gateway["mirrored"],
        "skipped_in_flight": gateway["skipped_in_flight"],
        "publish_failed": gateway["publish_failed"],
        "latency_ms": {
            "p50": round(values["med"], 3),
            "p95": round(values["p(95)"], 3),
            "p99": round(values["p(99)"], 3),
            "avg": round(values["avg"], 3),
            "max": round(values["max"], 3),
        },
        "resources": resources.get("containers", {}),
    }

    (out / f"{run}.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    gw = (result["resources"].get("gateway") or {}).get("cpu_seconds")
    print(f"{run}: {total} requests, {gateway['mirrored']} mirrored, "
          f"p50 {result['latency_ms']['p50']}ms p95 {result['latency_ms']['p95']}ms, gateway cpu {gw}s")


if __name__ == "__main__":
    main()
