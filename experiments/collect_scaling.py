"""Merge one scaling run into a result file.

    python experiments/collect_scaling.py scaling-2workers results/pr4 2 0

The question is whether a second comparison worker does half the work, so
the per-worker counts come from the workers' own logs: every processed event
is logged with its request id by the container that handled it. That is a
direct count rather than an inference from offsets.

A request id seen by both workers is a duplicate delivery, which at-least-once
allows. A request id stored twice would be a real fault, so the two are
counted separately.
"""

import json
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

REQUEST_ID = re.compile(r"request_id=([A-Za-z0-9]+)")
# docker compose prefixes each line with the container name.
CONTAINER = re.compile(r"^(\S*comparison-worker\S*)\s*\|")


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def per_worker(log_path):
    seen = defaultdict(set)
    if not log_path.exists():
        return seen
    for line in log_path.read_text(encoding="utf-8", errors="replace").splitlines():
        container = CONTAINER.match(line)
        request = REQUEST_ID.search(line)
        if container and request:
            seen[container.group(1).strip()].add(request.group(1))
    return seen


# TOPIC PARTITION CURRENT-OFFSET LOG-START-OFFSET LOG-END-OFFSET LAG ...
# The member and client ids that follow contain spaces, so the line is read
# from both ends rather than split into fields: the host at the end is what
# tells the two workers apart.
ASSIGNMENT = re.compile(r"^(mirrorgate\.\S+)\s+(\d+)\s+\S+\s+\S+\s+\S+\s+(\d+)\s+.*?(\S+)$")


def partitions_held(group_path):
    """Partitions assigned to each member, from rpk group describe."""
    held = Counter()
    if not group_path.exists():
        return held
    for line in group_path.read_text(encoding="utf-8", errors="replace").splitlines():
        match = ASSIGNMENT.match(line.strip())
        if match:
            held[match.group(4)] += 1
    return held


def main():
    label, out_dir, workers, rebalance = sys.argv[1:5]
    out = Path(out_dir)
    workers = int(workers)

    k6 = load(out / f"{label}.k6.json")
    stats = load(out / f".{label}.stats.json")
    db = load(out / f".{label}.db.json")
    resources_path = out / f"{label}.resources.json"
    resources = load(resources_path).get("containers", {}) if resources_path.exists() else {}

    handled = per_worker(out / f".{label}.worker.log")
    counts = {name: len(ids) for name, ids in sorted(handled.items())}
    everything = set()
    overlap = set()
    for ids in handled.values():
        overlap |= everything & ids
        everything |= ids

    mirrored = stats["gateway"]["mirrored"]
    stored = db["stored"]
    distinct = db["distinct_request_ids"]

    result = {
        "label": label,
        "workers_configured": workers,
        "workers_that_did_work": sum(1 for n in counts.values() if n > 0),
        "rebalance_during_run": rebalance == "1",
        "workload": {
            "rate_per_s": float(k6["metrics"]["http_reqs"]["values"]["rate"]),
            "total_requests": int(k6["metrics"]["http_reqs"]["values"]["count"]),
        },
        "mirrored": mirrored,
        "stored": stored,
        "distinct_request_ids": distinct,
        "complete": stored == mirrored,
        "duplicate_rows": stored - distinct,
        "events_handled_per_worker": counts,
        "events_handled_total": sum(counts.values()),
        "request_ids_handled_twice": len(overlap),
        "partitions_per_member": dict(partitions_held(out / f".{label}.group.txt")),
        "latency_ms": {
            "p50": round(k6["metrics"]["http_req_duration"]["values"]["med"], 3),
            "p95": round(k6["metrics"]["http_req_duration"]["values"]["p(95)"], 3),
        },
        "cpu_seconds": {
            name: round(info["cpu_seconds"], 4)
            for name, info in sorted(resources.items())
            if info.get("cpu_seconds") is not None
        },
    }
    # The resource monitor takes its list of containers when it starts, so a
    # worker that joins mid-run is not in it. Say so rather than letting the
    # number read as the cost of both workers.
    if rebalance == "1" and not any("#" in name for name in result["cpu_seconds"]):
        result["cpu_note"] = ("the second worker joined after the monitor started, "
                              "so its CPU is not included")

    path = out / f"{label}.json"
    path.write_text(json.dumps(result, indent=1) + "\n", encoding="utf-8")

    share = ""
    if len(counts) > 1 and sum(counts.values()):
        shares = [100 * n / sum(counts.values()) for n in counts.values()]
        share = "  split " + " / ".join(f"{s:.0f}%" for s in shares)
    print(f"\n{label}: {workers} worker(s) configured, "
          f"{result['workers_that_did_work']} did work{share}")
    for name, n in counts.items():
        print(f"  {name}: {n} events")
    print(f"  mirrored {mirrored}, stored {stored}, "
          f"{'complete' if result['complete'] else 'INCOMPLETE'}")
    print(f"  duplicate stored rows {result['duplicate_rows']}, "
          f"request ids delivered to both workers {result['request_ids_handled_twice']}")
    print(f"  -> {path}")


if __name__ == "__main__":
    main()
