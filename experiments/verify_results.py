"""Independent check of the rare-fault results, straight from the raw run files.

    python experiments/verify_results.py

Nothing here reads summary.csv or summary.md. Everything is recomputed from
the per-run JSON so the summaries can be checked rather than trusted.

What it checks:
  1. where the faults actually landed, and whether that was the same in every
     trial and every strategy
  2. the exact detection delays in milliseconds, not just request positions
  3. detection percentages and Wilson intervals, recomputed
  4. the risk-priority against random 10% comparison, treating a trial rather
     than a single fault as the unit of observation, because the five faults
     inside one trial share a sampler and are not independent of each other
  5. whether the CPU and memory numbers are comparable between arms
"""

import json
import math
import statistics
from collections import defaultdict
from pathlib import Path

FAULTS = ["R1", "R2", "R3", "R4", "R5"]
EXPECTED_SLOT = {"R1": 100, "R2": 300, "R3": 500, "R4": 700, "R5": 900}
BLOCK = 1000
ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint", "risk"]
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
    """95% interval for p1 - p2, built from the two Wilson intervals."""
    p1, p2 = s1 / n1, s2 / n2
    l1, u1 = wilson(s1, n1)
    l2, u2 = wilson(s2, n2)
    low = (p1 - p2) - math.sqrt((p1 - l1) ** 2 + (u2 - p2) ** 2)
    high = (p1 - p2) + math.sqrt((u1 - p1) ** 2 + (p2 - l2) ** 2)
    return p1 - p2, low, high


def welch(a, b, t=2.306):
    """Difference of means b - a with a 95% interval, unequal variances."""
    if len(a) < 2 or len(b) < 2:
        return statistics.fmean(b) - statistics.fmean(a), None, None
    diff = statistics.fmean(b) - statistics.fmean(a)
    se = math.sqrt(statistics.variance(a) / len(a) + statistics.variance(b) / len(b))
    return diff, diff - t * se, diff + t * se


def load():
    runs = defaultdict(list)
    for path in sorted(Path("results/rare-faults").glob("*.json")):
        if path.name in ("summary.json",) or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if "strategy" in run:
            runs[run["strategy"]].append(run)
    for trials in runs.values():
        trials.sort(key=lambda r: r["trial"])
    return runs


def check_positions(runs):
    print("=" * 70)
    print("1. WHERE THE FAULTS LANDED")
    print("=" * 70)
    print("The workload places faults at fixed offsets inside every block of")
    print("1000 requests, so the position is the same in every trial and for")
    print("every strategy. Detections should always sit on those offsets.\n")

    bad = []
    observed = defaultdict(set)
    for strategy, trials in runs.items():
        for run in trials:
            for fault in FAULTS:
                seq = run["detection"]["per_fault"][fault]["requests_until_detection"]
                if seq is None:
                    continue
                # request_seq counts from 1, the workload counts iterations
                # from 0, so a fault at offset 100 is request 101.
                offset = (seq - 1) % BLOCK
                observed[fault].add(offset)
                if offset != EXPECTED_SLOT[fault]:
                    bad.append((run["run"], fault, seq, offset))

    for fault in FAULTS:
        offsets = sorted(observed[fault])
        print(f"  {fault}: detected only at offset(s) {offsets}, expected {EXPECTED_SLOT[fault]}")
    print(f"\n  detections landing off the expected offset: {len(bad)}")
    if bad:
        for row in bad[:5]:
            print(f"    {row}")

    first = runs["full"][0]
    same_every_trial = all(
        run["detection"]["per_fault"][f]["requests_until_detection"]
        == first["detection"]["per_fault"][f]["requests_until_detection"]
        for run in runs["full"] for f in FAULTS
    )
    print(f"  full sampling caught every fault at the same position in all "
          f"{len(runs['full'])} trials: {same_every_trial}")
    print("  -> the 101/301/501/701/901 figures are the workload's fixed")
    print("     positions, not a coincidence and not a measurement of spread.")


def check_times(runs):
    print("\n" + "=" * 70)
    print("2. EXACT DETECTION DELAYS, IN MILLISECONDS")
    print("=" * 70)
    print("Measured from the first mirrored request of the run to the")
    print("detecting one, both on the gateway clock.\n")
    print(f"  {'strategy':<12} " + " ".join(f"{f:>14}" for f in FAULTS))
    for name in ORDER:
        if name not in runs:
            continue
        cells = []
        for fault in FAULTS:
            values = [r["detection"]["per_fault"][fault]["detection_delay_ms"]
                      for r in runs[name]
                      if r["detection"]["per_fault"][fault]["detection_delay_ms"] is not None]
            cells.append(f"{statistics.fmean(values):>10.0f} ms" if values else f"{'never':>14}")
        print(f"  {name:<12} " + " ".join(cells))

    print("\n  sanity check: delay should be about (position - 1) / rate")
    for fault in FAULTS:
        values = [(r["detection"]["per_fault"][fault]["requests_until_detection"],
                   r["detection"]["per_fault"][fault]["detection_delay_ms"])
                  for r in runs["full"]]
        seq, delay = values[0]
        print(f"    {fault}: request {seq} at 100/s -> expected ~{(seq - 1) / 100 * 1000:.0f} ms, "
              f"measured {delay:.0f} ms")


def check_detection(runs):
    print("\n" + "=" * 70)
    print("3. DETECTION PERCENTAGES, RECOMPUTED FROM THE RAW FILES")
    print("=" * 70)
    for name in ORDER:
        if name not in runs:
            continue
        trials = runs[name]
        found = sum(1 for r in trials for f in FAULTS if r["detection"]["per_fault"][f]["detected"])
        total = len(trials) * len(FAULTS)
        low, high = wilson(found, total)
        rate = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in trials)
        print(f"  {name:<12} {found:>3}/{total}  {found/total*100:5.1f}%  "
              f"Wilson {low*100:4.0f}-{high*100:3.0f}%   mirrored {rate:5.1f}%")

    print("\n  per trial, faults detected out of 5:")
    for name in ORDER:
        if name not in runs:
            continue
        counts = [r["detection"]["faults_detected"] for r in runs[name]]
        print(f"  {name:<12} {counts}  mean {statistics.fmean(counts):.1f}")


def check_risk_comparison(runs):
    print("\n" + "=" * 70)
    print("4. RISK-PRIORITY AGAINST RANDOM 10%")
    print("=" * 70)
    risk, rand = runs["risk"], runs["random-10"]

    risk_counts = [r["detection"]["faults_detected"] for r in risk]
    rand_counts = [r["detection"]["faults_detected"] for r in rand]
    print(f"  risk      faults per trial: {risk_counts}")
    print(f"  random 10 faults per trial: {rand_counts}")

    print("\n  Treating each trial as one observation, which is the honest unit:")
    print("  the five faults inside a trial share one gateway, one sampler and")
    print("  one risk state, so they are not five independent coin flips.")
    diff, low, high = welch(rand_counts, risk_counts)
    verdict = "separated" if low is not None and low > 0 else "not separated at this sample size"
    print(f"    difference in faults per trial: {diff:+.1f} "
          f"(95% CI {low:+.1f} to {high:+.1f}) -> {verdict}")

    # Paired view: trials ran in the same loop, so trial n shared machine
    # conditions between the two arms.
    pairs = [r - n for r, n in zip(risk_counts, rand_counts)]
    mean_diff = statistics.fmean(pairs)
    if len(pairs) > 1 and statistics.stdev(pairs) > 0:
        se = statistics.stdev(pairs) / math.sqrt(len(pairs))
        lo, hi = mean_diff - 2.262 * se, mean_diff + 2.262 * se
        paired = "separated" if lo > 0 else "not separated"
        print(f"    paired by trial number:         {mean_diff:+.1f} "
              f"(95% CI {lo:+.1f} to {hi:+.1f}) -> {paired}")

    print("\n  Pooling all 50 fault chances per arm as if independent would")
    print("  give a narrower interval than the data supports. For reference:")
    risk_found = sum(risk_counts)
    rand_found = sum(rand_counts)
    rl, rh = wilson(risk_found, 50)
    nl, nh = wilson(rand_found, 50)
    print(f"    risk {risk_found}/50 = {risk_found*2}% (Wilson {rl*100:.0f}-{rh*100:.0f}), "
          f"random {rand_found}/50 = {rand_found*2}% (Wilson {nl*100:.0f}-{nh*100:.0f})")

    print("\n  Per fault the clustering problem goes away: one fault is one")
    print("  independent outcome per trial. The right test is an interval on")
    print("  the difference, not whether the two separate intervals happen to")
    print("  overlap, which is a different and more conservative comparison.")
    for fault in FAULTS:
        rs = sum(1 for r in risk if r["detection"]["per_fault"][fault]["detected"])
        ns = sum(1 for r in rand if r["detection"]["per_fault"][fault]["detected"])
        d, lo, hi = newcombe(rs, len(risk), ns, len(rand))
        verdict = "separated" if lo > 0 or hi < 0 else "not separated"
        print(f"    {fault}: risk {rs}/10 vs random {ns}/10, difference "
              f"{d*100:+.0f} points (95% CI {lo*100:+.0f} to {hi*100:+.0f}) -> {verdict}")


def check_resources(runs):
    print("\n" + "=" * 70)
    print("5. ARE THE RESOURCE NUMBERS COMPARABLE")
    print("=" * 70)
    print("  CPU is a counter difference over the measured window, so the")
    print("  windows have to be about the same length for the arms to compare.\n")
    for name in ORDER:
        if name not in runs:
            continue
        trials = runs[name]
        cpu = [sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS) for r in trials]
        samples = [(r["resources"].get("gateway") or {}).get("memory_samples") or 0 for r in trials]
        missing = sum(1 for r in trials for c in CONTAINERS
                      if (r["resources"].get(c) or {}).get("cpu_seconds") is None)
        requests = [r["workload"]["total_requests"] for r in trials]
        print(f"  {name:<12} cpu {statistics.fmean(cpu):5.2f}s (sd {statistics.stdev(cpu):4.2f})  "
              f"mem samples {statistics.fmean(samples):4.1f}  requests {statistics.fmean(requests):.0f}  "
              f"missing cpu readings {missing}")

    print("\n  negative or zero CPU deltas (would mean a restart mid-window):")
    bad = [(r["run"], c) for trials in runs.values() for r in trials for c in CONTAINERS
           if ((r["resources"].get(c) or {}).get("cpu_seconds") or 0) <= 0]
    print(f"    {len(bad)} found" + (f": {bad[:4]}" if bad else ""))


def check_validity(runs):
    print("\n" + "=" * 70)
    print("6. RUN VALIDITY AND SIDE EFFECTS")
    print("=" * 70)
    total = sum(len(v) for v in runs.values())
    invalid = [r["run"] for v in runs.values() for r in v if not r.get("valid", True)]
    isolation = sum(1 for v in runs.values() for r in v if r["side_effects"]["isolation_ok"])
    publish_failed = sum(r["sampling"]["publish_failed"] for v in runs.values() for r in v)
    skipped = sum(r["sampling"]["skipped_in_flight"] for v in runs.values() for r in v)
    pending = sum(r["outbox"].get("pending", 0) for v in runs.values() for r in v)
    print(f"  runs: {total}, invalid: {len(invalid)} {invalid}")
    print(f"  shadow write isolation held: {isolation}/{total}")
    print(f"  publish failures across every run: {publish_failed}")
    print(f"  shadow requests dropped for the in-flight limit: {skipped}")
    print(f"  outbox rows still pending when results were read: {pending}")

    print("\n  stored comparisons against what the gateway says it mirrored:")
    mismatch = [(r["run"], r["sampling"]["mirrored"], r["stored"]["total"])
                for v in runs.values() for r in v
                if r["sampling"]["mirrored"] != r["stored"]["total"]]
    print(f"    runs where the two disagree: {len(mismatch)}")
    for row in mismatch[:5]:
        print(f"      {row[0]}: mirrored {row[1]}, stored {row[2]}, difference {row[1] - row[2]}")


def main():
    runs = load()
    check_positions(runs)
    check_times(runs)
    check_detection(runs)
    check_risk_comparison(runs)
    check_resources(runs)
    check_validity(runs)


if __name__ == "__main__":
    main()
