"""Merge one rare-fault run into a single result file.

    python experiments/collect_rare.py risk-t1 results/rare-faults 100 30s risk outbox

Four sources have to agree: k6 knows what the client sent and how long it
took, the gateway knows how much it mirrored and when it started, Postgres
knows what the worker made of it, and the resource monitor knows what it cost.

On clocks. Three different times are involved and they are not
interchangeable:

  request_seq   the position the gateway gave the request, counted from its
                own restart. An integer, not a time, and the exact answer to
                "how much traffic went by before the fault was caught".
  received_at   when the gateway received the fault-bearing request. Gateway
                clock, the same clock as started_at, so subtracting them is
                safe. This is an arrival time, not a detection time.
  detected_at   when the worker finished comparing and stored the row, which
                is when the regression was actually confirmed. Worker clock.

The difference between the last two is the thing that is easy to get wrong.
A fault-bearing request arriving does not mean the regression is known yet:
the candidate still has to answer (R4 deliberately takes the full two second
timeout), the event goes through the outbox, the relay, the topic and the
worker. So arrival and confirmation are reported as separate numbers rather
than one called "detection".
"""

import json
import statistics
import sys
from datetime import datetime
from pathlib import Path

# How far a request's arrival position may sit from its iteration number
# before the two are treated as unrelated. See the comment where it is used.
DRIFT = 25

FAULTS = {
    "R1": "price mismatch on /api/products/12",
    "R2": "candidate 500 on /api/products/8",
    "R3": "slow /api/products?category=audio",
    "R4": "timeout on /api/users/7",
    "R5": "wrong total on POST /api/orders",
}


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def parse_time(value):
    if not value:
        return None
    # Postgres gives microseconds and a +00 offset; Go gives RFC3339 with Z.
    text = value.replace("Z", "+00:00")
    if text.endswith("+00"):
        text += ":00"
    return datetime.fromisoformat(text)


def main():
    run, out_dir, rate, duration, mode, publish_mode = sys.argv[1:7]
    out = Path(out_dir)
    rate = float(rate)

    k6 = load(out / f"{run}.k6.json")
    stats = load(out / f"{run}.stats.json")
    db = load(out / f"{run}.db.json")
    sampling = load(out / f"{run}.sampling.json")
    external = load(out / f"{run}.external.json")
    resources_path = out / f"{run}.resources.json"
    resources = load(resources_path) if resources_path.exists() else {}

    metrics = k6["metrics"]
    duration_ms = metrics["http_req_duration"]["values"]
    total_requests = int(metrics["http_reqs"]["values"]["count"])
    gateway = stats["gateway"]
    mirrored = gateway["mirrored"]
    eligible = gateway["eligible"]
    started_at = parse_time(gateway["started_at"])

    # The time metric is measured from the first mirrored request of the run,
    # not from the gateway process start: the harness restarts the gateway and
    # then waits for health checks and for k6 to start, and those seconds are
    # not part of detecting anything. Both timestamps are the gateway clock.
    run_start = parse_time(db.get("run_first_received_at"))

    detection = {}
    for fault in FAULTS:
        row = db["faults"].get(fault) or {}
        hits = row.get("hits") or 0
        first_seq = row.get("first_seq")
        first_at = parse_time(row.get("first_received_at"))
        confirmed_at = parse_time(row.get("first_detected_at"))
        arrival_ms = None
        if first_at is not None and run_start is not None:
            arrival_ms = round((first_at - run_start).total_seconds() * 1000, 1)
        confirmation_ms = None
        if confirmed_at is not None and run_start is not None:
            confirmation_ms = round((confirmed_at - run_start).total_seconds() * 1000, 1)
        pipeline_ms = None
        if confirmed_at is not None and first_at is not None:
            pipeline_ms = round((confirmed_at - first_at).total_seconds() * 1000, 1)
        since_boot_ms = None
        if first_at is not None and started_at is not None:
            since_boot_ms = round((first_at - started_at).total_seconds() * 1000, 1)
        detection[fault] = {
            "detected": hits > 0,
            "hits": hits,
            # Exact position in the gateway's eligible request stream. This is
            # the primary number: it is a count, with no clock involved.
            "requests_until_detection": first_seq,
            # When the fault-bearing request arrived, measured from the first
            # mirrored request of the run. Gateway clock. NOT a detection time.
            "arrival_delay_ms": arrival_ms,
            # When the worker confirmed the regression, same anchor. This is
            # the one that answers "how long until MirrorGate knew".
            "confirmation_delay_ms": confirmation_ms,
            # How long the candidate call plus the pipeline took for this one.
            "pipeline_delay_ms": pipeline_ms,
            # Same clock, measured from the gateway process start. Included
            # for completeness; it contains the harness startup gap.
            "delay_since_gateway_start_ms": since_boot_ms,
            "description": FAULTS[fault],
        }

    found = sum(1 for d in detection.values() if d["detected"])

    # The randomised workload writes the positions it drew next to the
    # results. With it, a detection can be tied to which occurrence of the
    # fault was caught rather than only to the fault.
    plan_path = out / f"{run}.faults.json"
    fault_plan = None
    if plan_path.exists():
        plan = load(plan_path)
        per_fault = {}
        offsets = []
        off_plan = []
        for fault in FAULTS:
            expected = plan["expected_request_seq"][fault]
            caught = [int(x) for x in (db["faults"][fault].get("seqs") or [])]
            first = detection[fault]["requests_until_detection"]

            # Each caught row belongs to the planned position nearest it. The
            # two numbers are usually equal, but k6 spreads iterations over 50
            # virtual users, so a request can reach the gateway a few places
            # from where its iteration number says. The planned positions of
            # one fault are hundreds of requests apart, so nearest wins by a
            # wide margin; anything further than DRIFT away is reported
            # instead of being matched.
            matched = []
            for seq in caught:
                nearest = min(expected, key=lambda e: abs(e - seq))
                offsets.append(seq - nearest)
                if abs(seq - nearest) > DRIFT:
                    off_plan.append(f"{fault}@{seq}")
                else:
                    matched.append(expected.index(nearest) + 1)

            index = min(matched) if matched else None
            per_fault[fault] = {
                "expected_request_seq": expected,
                "caught_request_seq": caught,
                # Which occurrence was the first one caught, counting from 1.
                "first_caught_occurrence": index,
                "occurrences_caught": len(matched),
                "requests_after_first_occurrence":
                    first - expected[0] if first is not None else None,
            }
        fault_plan = {
            "seed": plan["seed"],
            "per_fault_occurrences": plan["per_fault"],
            "faults": per_fault,
            "max_position_drift": max((abs(o) for o in offsets), default=0),
            # Anything here means a detection could not be matched to a
            # planned position at all, which would invalidate the positions.
            "detections_off_plan": off_plan,
        }

    # A run where events were lost or shadow slots ran out is not a clean
    # measurement of sampling, so flag it rather than averaging it in.
    invalid = []
    if gateway["publish_failed"] > 0:
        invalid.append(f"publish_failed={gateway['publish_failed']}")
    if gateway["skipped_in_flight"] > 0:
        invalid.append(f"skipped_in_flight={gateway['skipped_in_flight']}")

    if fault_plan:
        orders_sent = len(fault_plan["faults"]["R5"]["expected_request_seq"])
    else:
        orders_sent = total_requests // 1000 + (1 if total_requests % 1000 > 900 else 0)
    counts = external.get("counts", {})

    deltas = [float(d) for d in db.get("clean_latency_deltas", [])]
    deltas.sort()

    result = {
        "run": run,
        "strategy": run.rsplit("-t", 1)[0],
        "trial": int(run.rsplit("-t", 1)[1]),
        "mode": mode,
        "publish_mode": publish_mode,
        "valid": not invalid,
        "invalid_reasons": invalid,
        "workload": {
            "script": "experiments/rare-random.js" if fault_plan else "experiments/rare.js",
            "rate_per_s": rate,
            "duration": duration,
            "total_requests": total_requests,
            "fault_every": None if fault_plan else 1000,
        },
        "sampling": {
            "mode": stats["sampling"]["mode"],
            # Recorded rather than inferred from the arm name, so a run file
            # says which configuration actually produced it.
            "route_templates": stats["sampling"].get("route_templates"),
            "eligible": eligible,
            "mirrored": mirrored,
            "skipped": stats["sampling"]["skipped"],
            "actual_rate_pct": round(100 * mirrored / eligible, 2) if eligible else 0.0,
            "published": gateway["published"],
            "publish_failed": gateway["publish_failed"],
            "skipped_in_flight": gateway["skipped_in_flight"],
            "endpoints": sampling.get("endpoints", []),
        },
        "outbox": stats.get("outbox", {}),
        "stored": {
            "total": db["stored"],
            "matched": db["matched"],
            "different": db["different"],
            "errors": db["errors"],
            "slow": db["slow"],
            "orders_mirrored": db["orders_mirrored"],
            "max_request_seq": db["max_seq"],
        },
        "detection": {
            "faults_detected": found,
            "faults_total": len(FAULTS),
            "per_fault": detection,
        },
        "fault_plan": fault_plan,
        "latency_ms": {
            "p50": round(duration_ms["med"], 3),
            "p95": round(duration_ms["p(95)"], 3),
            "p99": round(duration_ms["p(99)"], 3),
            "avg": round(duration_ms["avg"], 3),
            "max": round(duration_ms["max"], 3),
        },
        # Used to justify the slow threshold: what the candidate-minus-stable
        # difference looks like on endpoints that are not supposed to be slow.
        "clean_latency_delta_ms": {
            "count": len(deltas),
            "p50": round(statistics.median(deltas), 3) if deltas else None,
            "p99": round(deltas[min(len(deltas) - 1, int(0.99 * len(deltas)))], 3) if deltas else None,
            "max": round(deltas[-1], 3) if deltas else None,
        },
        "resources": resources.get("containers", {}),
        "resource_method": resources.get("method", {}),
        "side_effects": {
            "orders_sent_by_client": orders_sent,
            "orders_mirrored": db["orders_mirrored"],
            "calls": counts,
            "isolation_ok": counts.get("live_payment", 0) == orders_sent
                            and counts.get("sandbox_payment", 0) == db["orders_mirrored"],
        },
    }

    path = out / f"{run}.json"
    path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")

    gw_cpu = (resources.get("containers", {}).get("gateway") or {}).get("cpu_seconds")
    print(
        f"{run}: {total_requests} requests, {mirrored} mirrored "
        f"({result['sampling']['actual_rate_pct']}%), "
        f"{found}/{len(FAULTS)} faults, p95 {result['latency_ms']['p95']}ms, "
        f"gateway cpu {gw_cpu}s -> {path}"
    )
    if invalid:
        print(f"  WARNING: run marked invalid ({', '.join(invalid)})")
    if not result["side_effects"]["isolation_ok"]:
        print(f"  WARNING: side effects do not line up: {counts}")


if __name__ == "__main__":
    main()
