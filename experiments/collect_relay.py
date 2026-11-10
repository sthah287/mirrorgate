"""Merge one relay throughput run into a result file.

    python experiments/collect_relay.py before results/pr4 100 30s 118

The interesting number is not how fast the gateway accepted requests, it is
how far behind the comparison pipeline fell and how long it took to catch up
once the load stopped.
"""

import json
import re
import sys
from pathlib import Path


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def seconds(duration):
    """k6 style duration, "30s" or "2m30s"."""
    total = 0
    for value, unit in re.findall(r"(\d+(?:\.\d+)?)([hms])", duration):
        total += float(value) * {"h": 3600, "m": 60, "s": 1}[unit]
    return total or float(duration)


def main():
    label, out_dir, rate, duration, drain_seconds = sys.argv[1:6]
    out = Path(out_dir)
    rate = float(rate)
    drain_seconds = int(drain_seconds)

    k6 = load(out / f"{label}.k6.json")
    stats = load(out / f".{label}.stats.json")
    lag = load(out / f".{label}.lag.json")
    resources_path = out / f"{label}.resources.json"
    resources = load(resources_path).get("containers", {}) if resources_path.exists() else {}

    depth = []
    depth_path = out / f".{label}.depth"
    if depth_path.exists():
        for line in depth_path.read_text(encoding="utf-8").splitlines():
            parts = line.split()
            if len(parts) == 3:
                depth.append({"t": int(parts[0]), "pending": int(parts[1]), "stored": int(parts[2])})

    peak = max((d["pending"] for d in depth), default=0)
    peak_at = next((d["t"] for d in depth if d["pending"] == peak), None)
    total = int(k6["metrics"]["http_reqs"]["values"]["count"])
    mirrored = stats["gateway"]["mirrored"]
    stored = lag["stored"]

    # Throughput while the queue was clearing: everything that was still
    # backed up when the load stopped, divided by how long it took to go.
    # Only meaningful when there was a backlog to clear.
    drain_throughput = round(peak / drain_seconds, 1) if drain_seconds > 0 and peak else None
    # Confirmed events per second across the load plus the catch-up that
    # followed it. Wall-clock seconds, not depth samples: each sample costs two
    # psql round trips, so the samples are slower than one a second. When the
    # pipeline keeps up this is bounded by the offered rate, not by the relay.
    window = seconds(duration) + drain_seconds
    confirmed = round(stored / window, 1) if window else None

    result = {
        "label": label,
        "workload": {"rate_per_s": rate, "duration": duration, "total_requests": total},
        "mirrored": mirrored,
        "stored": stored,
        "complete": mirrored == stored,
        "backlog": {
            "peak_pending": peak,
            "peak_at_second": peak_at,
            "drain_seconds_after_load": drain_seconds,
            "drain_throughput_per_s": drain_throughput,
            "confirmed_throughput_per_s": confirmed,
            "confirmed_window_seconds": window,
        },
        "confirmation_lag_ms": {
            "p50": lag["lag_p50_ms"],
            "p95": lag["lag_p95_ms"],
            "max": lag["lag_max_ms"],
            "min": lag["first_seq_lag_ms"],
        },
        "latency_ms": {
            "p50": round(k6["metrics"]["http_req_duration"]["values"]["med"], 3),
            "p95": round(k6["metrics"]["http_req_duration"]["values"]["p(95)"], 3),
            "p99": round(k6["metrics"]["http_req_duration"]["values"]["p(99)"], 3),
        },
        "cpu_seconds": {k: (v or {}).get("cpu_seconds") for k, v in resources.items()},
        "memory_peak_mb": {k: (v or {}).get("mem_peak_mb") for k, v in resources.items()},
        "outbox": stats.get("outbox", {}),
        "publish_failed": stats["gateway"]["publish_failed"],
        "depth_curve": depth,
    }

    path = out / f"relay-{label}.json"
    path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")

    print(f"\n{label}: {total} requests, {mirrored} mirrored, {stored} stored "
          f"({'complete' if result['complete'] else 'INCOMPLETE'})")
    print(f"  peak backlog      {peak} events at t={peak_at}s")
    print(f"  drain after load  {drain_seconds}s  ({drain_throughput}/s)")
    print(f"  confirmation lag  p50 {lag['lag_p50_ms']}ms  p95 {lag['lag_p95_ms']}ms  "
          f"max {lag['lag_max_ms']}ms")
    print(f"  relay cpu         {result['cpu_seconds'].get('outbox-relay')}s")
    print(f"  -> {path}")


if __name__ == "__main__":
    main()
