"""Check the claims the report wants to make about the rare-fault results.

    python experiments/compare_strategies.py

Three questions:

1. Risk-priority and random 10% mirror about the same amount of traffic. Is
   the difference in what they catch big enough to call a difference, or is it
   inside the noise of ten trials?
2. Detection should follow P = 1 - (1 - rate)^k with k = 3 occurrences per
   trial. Does it?
3. Any single trial distorting a mean, so the report can say so instead of
   quoting it as if it were typical.

The interval for the difference of two proportions is Newcombe's, built from
the two Wilson intervals. With ten trials per arm it is wide, and if it
contains zero the report says the experiment did not separate them.
"""

import json
import math
import statistics
from pathlib import Path

FAULTS = ["R1", "R2", "R3", "R4", "R5"]
CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]


def wilson(successes, total, z=1.96):
    if total == 0:
        return 0.0, 0.0
    p = successes / total
    denom = 1 + z * z / total
    centre = (p + z * z / (2 * total)) / denom
    margin = z * math.sqrt(p * (1 - p) / total + z * z / (4 * total * total)) / denom
    return max(0.0, centre - margin), min(1.0, centre + margin)


def newcombe(s1, n1, s2, n2):
    """95% interval for p1 - p2, from the two Wilson intervals."""
    p1, p2 = s1 / n1, s2 / n2
    l1, u1 = wilson(s1, n1)
    l2, u2 = wilson(s2, n2)
    low = (p1 - p2) - math.sqrt((p1 - l1) ** 2 + (u2 - p2) ** 2)
    high = (p1 - p2) + math.sqrt((u1 - p1) ** 2 + (p2 - l2) ** 2)
    return p1 - p2, low, high


def load():
    runs = {}
    for path in sorted(Path("results/rare-faults").glob("*.json")):
        if path.name in ("summary.json",) or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if run.get("valid", True):
            runs.setdefault(run["strategy"], []).append(run)
    return runs


def detections(trials):
    found = sum(1 for r in trials for f in FAULTS if r["detection"]["per_fault"][f]["detected"])
    return found, len(trials) * len(FAULTS)


def main():
    runs = load()

    print("=== risk-priority against random 10%, which cost about the same ===")
    risk_s, risk_n = detections(runs["risk"])
    rand_s, rand_n = detections(runs["random-10"])
    risk_rate = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in runs["risk"])
    rand_rate = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in runs["random-10"])
    risk_cpu = statistics.fmean(
        sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS) for r in runs["risk"])
    rand_cpu = statistics.fmean(
        sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS) for r in runs["random-10"])

    print(f"  risk      : {risk_s}/{risk_n} caught, {risk_rate:.1f}% mirrored, {risk_cpu:.2f} CPU s")
    print(f"  random 10%: {rand_s}/{rand_n} caught, {rand_rate:.1f}% mirrored, {rand_cpu:.2f} CPU s")
    diff, low, high = newcombe(risk_s, risk_n, rand_s, rand_n)
    verdict = "separated" if low > 0 else "not separated by this experiment"
    print(f"  difference: {diff*100:+.0f} points (95% CI {low*100:+.0f} to {high*100:+.0f}) -> {verdict}")

    print("\n  per fault:")
    for fault in FAULTS:
        rs = sum(1 for r in runs["risk"] if r["detection"]["per_fault"][fault]["detected"])
        ns = sum(1 for r in runs["random-10"] if r["detection"]["per_fault"][fault]["detected"])
        d, lo, hi = newcombe(rs, len(runs["risk"]), ns, len(runs["random-10"]))
        mark = "*" if lo > 0 or hi < 0 else " "
        print(f"   {mark} {fault}: risk {rs}/10 vs random {ns}/10, "
              f"difference {d*100:+.0f} points (CI {lo*100:+.0f} to {hi*100:+.0f})")

    print("\n=== observed detection against the theory P = 1 - (1 - rate)^3 ===")
    for name in ["full", "random-50", "random-25", "random-10"]:
        found, total = detections(runs[name])
        rate = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in runs[name]) / 100
        predicted = 1 - (1 - rate) ** 3
        low_ci, high_ci = wilson(found, total)
        inside = low_ci <= predicted <= high_ci
        print(f"  {name:<10} rate {rate*100:5.1f}%  predicted {predicted*100:5.1f}%  "
              f"observed {found/total*100:5.1f}% ({low_ci*100:.0f}-{high_ci*100:.0f})  "
              f"{'theory inside the interval' if inside else 'outside the interval'}")

    print("\n=== trials that pull a mean around ===")
    for name, trials in runs.items():
        p99s = [t["latency_ms"]["p99"] for t in trials]
        worst = max(p99s)
        median = statistics.median(p99s)
        if worst > 4 * median:
            run = next(t["run"] for t in trials if t["latency_ms"]["p99"] == worst)
            print(f"  {name}: p99 mean {statistics.fmean(p99s):.2f} ms is pulled by {run} at "
                  f"{worst:.2f} ms; the median trial is {median:.2f} ms")

    print("\n=== cost of a detection ===")
    for name in ["full", "random-50", "random-25", "endpoint", "random-10", "risk"]:
        found, _ = detections(runs[name])
        mirrored = statistics.fmean(r["sampling"]["mirrored"] for r in runs[name])
        if found:
            print(f"  {name:<10} {mirrored * len(runs[name]) / found:6.0f} mirrored requests per fault caught")


if __name__ == "__main__":
    main()
