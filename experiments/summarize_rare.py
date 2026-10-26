"""Summarise the rare-fault trials into summary.csv and summary.md.

    python experiments/summarize_rare.py results/rare-faults

Detection is now a coin flip per fault per trial rather than something that
always happens, so the headline number is a probability with an interval
around it. The interval is Wilson, not the normal approximation: with ten
trials the normal one gives nonsense near 0 and 1, including intervals that
run past the ends of the scale.

Nothing here is smoothed or filled in. A fault found in 3 of 10 trials is
reported as 30%, and runs flagged invalid by the collector are listed and left
out of the averages.
"""

import csv
import json
import math
import random
import statistics
import sys
from pathlib import Path

ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint", "risk"]
FAULTS = ["R1", "R2", "R3", "R4", "R5"]
LABELS = {
    "off": "off (0%)",
    "full": "full (100%)",
    "random-50": "random 50%",
    "random-25": "random 25%",
    "random-10": "random 10%",
    "endpoint": "endpoint rules",
    "risk": "risk-priority",
}
CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]


def cluster_interval(proportions, seed=20261026, rounds=20000):
    """95% interval that treats the trial as the unit of observation.

    The five faults inside one trial share a gateway, a sampler and, in risk
    mode, a risk state that climbs during the run, so they are not five
    independent chances. Resampling whole trials keeps that clustering intact.
    Returns the mean with its interval, and whether the interval collapsed
    because every trial gave the same answer.
    """
    n = len(proportions)
    mean = statistics.fmean(proportions)
    if n < 2:
        return mean, mean, mean, True
    if statistics.stdev(proportions) == 0:
        return mean, mean, mean, True
    rng = random.Random(seed)
    means = sorted(statistics.fmean(rng.choices(proportions, k=n)) for _ in range(rounds))
    return mean, means[int(0.025 * rounds)], means[int(0.975 * rounds)], False


def wilson(successes, total, z=1.96):
    """95% Wilson score interval. Behaves sensibly at 0 and at 1."""
    if total == 0:
        return 0.0, 0.0
    p = successes / total
    denom = 1 + z * z / total
    centre = (p + z * z / (2 * total)) / denom
    margin = z * math.sqrt(p * (1 - p) / total + z * z / (4 * total * total)) / denom
    return max(0.0, centre - margin), min(1.0, centre + margin)


def mean(values):
    return statistics.fmean(values) if values else None


def sd(values):
    return statistics.stdev(values) if len(values) > 1 else 0.0


def fmt(value, places=1):
    return "-" if value is None else f"{value:.{places}f}"


def main():
    out = Path(sys.argv[1] if len(sys.argv) > 1 else "results/rare-faults")
    runs = []
    for path in sorted(out.glob("*.json")):
        if path.name in ("summary.json",) or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        # results/ also holds analysis artefacts; a run file is the one with a
        # strategy on it.
        if "strategy" in run:
            runs.append(run)
    if not runs:
        print(f"no run files in {out}")
        return

    invalid = [r["run"] for r in runs if not r.get("valid", True)]
    runs = [r for r in runs if r.get("valid", True)]

    by_strategy = {}
    for run in runs:
        by_strategy.setdefault(run["strategy"], []).append(run)

    rows = []
    for name in ORDER + sorted(set(by_strategy) - set(ORDER)):
        trials = by_strategy.get(name)
        if not trials:
            continue
        trials.sort(key=lambda r: r["trial"])

        per_fault = {}
        for fault in FAULTS:
            hits = [t["detection"]["per_fault"][fault] for t in trials]
            found = sum(1 for h in hits if h["detected"])
            seqs = [h["requests_until_detection"] for h in hits if h["requests_until_detection"] is not None]
            # Runs collected before the timing fix only carry the old key,
            # which held the arrival time under a misleading name.
            delays = [h.get("arrival_delay_ms", h.get("detection_delay_ms"))
                      for h in hits
                      if h.get("arrival_delay_ms", h.get("detection_delay_ms")) is not None]
            confirms = [h["confirmation_delay_ms"] for h in hits
                        if h.get("confirmation_delay_ms") is not None]
            low, high = wilson(found, len(hits))
            per_fault[fault] = {
                "found": found,
                "trials": len(hits),
                "probability": found / len(hits),
                "ci_low": low,
                "ci_high": high,
                "mean_requests_until_detection": mean(seqs),
                "sd_requests_until_detection": sd(seqs),
                "mean_arrival_delay_ms": mean(delays),
                "mean_confirmation_delay_ms": mean(confirms),
            }

        total_found = sum(per_fault[f]["found"] for f in FAULTS)
        total_chances = sum(per_fault[f]["trials"] for f in FAULTS)
        pooled_low, pooled_high = wilson(total_found, total_chances)
        per_trial = [t["detection"]["faults_detected"] / len(FAULTS) for t in trials]
        _, cluster_low, cluster_high, collapsed = cluster_interval(per_trial)

        cpu = {c: mean([(t["resources"].get(c) or {}).get("cpu_seconds") or 0 for t in trials]) for c in CONTAINERS}
        mem = {c: mean([(t["resources"].get(c) or {}).get("mem_peak_mb") or 0 for t in trials]) for c in CONTAINERS}
        total_cpu = sum(v for v in cpu.values() if v)
        mirrored = mean([t["sampling"]["mirrored"] for t in trials])
        requests = mean([t["workload"]["total_requests"] for t in trials])

        rows.append({
            "strategy": name,
            "label": LABELS.get(name, name),
            "trials": len(trials),
            "mean_requests": requests,
            "mean_mirrored": mirrored,
            "mean_actual_rate_pct": mean([t["sampling"]["actual_rate_pct"] for t in trials]),
            "mean_faults_detected": mean([t["detection"]["faults_detected"] for t in trials]),
            "pooled_probability": total_found / total_chances if total_chances else 0,
            "pooled_ci": (pooled_low, pooled_high),
            "cluster_ci": (cluster_low, cluster_high),
            "cluster_collapsed": collapsed,
            "mean_p50_ms": mean([t["latency_ms"]["p50"] for t in trials]),
            "sd_p50_ms": sd([t["latency_ms"]["p50"] for t in trials]),
            "mean_p95_ms": mean([t["latency_ms"]["p95"] for t in trials]),
            "sd_p95_ms": sd([t["latency_ms"]["p95"] for t in trials]),
            "mean_p99_ms": mean([t["latency_ms"]["p99"] for t in trials]),
            "cpu": cpu,
            "mem": mem,
            "total_cpu_seconds": total_cpu,
            "cpu_per_1k_requests": total_cpu / (requests / 1000) if requests else None,
            # What each detection cost, which only makes sense where something
            # was detected at all.
            "mirrored_per_detection": (mirrored * len(trials) / total_found) if total_found else None,
            "cpu_per_detection": (total_cpu * len(trials) / total_found) if total_found else None,
            "per_fault": per_fault,
            "isolation_ok_trials": sum(1 for t in trials if t["side_effects"]["isolation_ok"]),
        })

    # ---- csv ----
    csv_path = out / "summary.csv"
    with csv_path.open("w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow([
            "strategy", "trials", "mean_requests", "mean_mirrored", "mean_actual_rate_pct",
            "mean_faults_detected", "pooled_detection_probability", "pooled_ci_low", "pooled_ci_high",
            "mean_p50_ms", "sd_p50_ms", "mean_p95_ms", "sd_p95_ms", "mean_p99_ms",
            "total_cpu_seconds", "cpu_per_1k_requests", "mirrored_per_detection", "cpu_per_detection",
        ] + [f"cpu_{c}" for c in CONTAINERS]
          + [f"mem_peak_{c}" for c in CONTAINERS]
          + [f"{f}_found" for f in FAULTS]
          + [f"{f}_probability" for f in FAULTS]
          + [f"{f}_ci_low" for f in FAULTS]
          + [f"{f}_ci_high" for f in FAULTS]
          + [f"{f}_mean_requests_until_detection" for f in FAULTS])
        for row in rows:
            writer.writerow([
                row["strategy"], row["trials"], f"{row['mean_requests']:.0f}", f"{row['mean_mirrored']:.0f}",
                f"{row['mean_actual_rate_pct']:.2f}", f"{row['mean_faults_detected']:.2f}",
                f"{row['pooled_probability']:.3f}", f"{row['pooled_ci'][0]:.3f}", f"{row['pooled_ci'][1]:.3f}",
                f"{row['mean_p50_ms']:.3f}", f"{row['sd_p50_ms']:.3f}", f"{row['mean_p95_ms']:.3f}",
                f"{row['sd_p95_ms']:.3f}", f"{row['mean_p99_ms']:.3f}",
                f"{row['total_cpu_seconds']:.2f}", fmt(row["cpu_per_1k_requests"], 2),
                fmt(row["mirrored_per_detection"], 1), fmt(row["cpu_per_detection"], 2),
            ] + [fmt(row["cpu"][c], 3) for c in CONTAINERS]
              + [fmt(row["mem"][c], 1) for c in CONTAINERS]
              + [row["per_fault"][f]["found"] for f in FAULTS]
              + [f"{row['per_fault'][f]['probability']:.2f}" for f in FAULTS]
              + [f"{row['per_fault'][f]['ci_low']:.2f}" for f in FAULTS]
              + [f"{row['per_fault'][f]['ci_high']:.2f}" for f in FAULTS]
              + [fmt(row["per_fault"][f]["mean_requests_until_detection"], 0) for f in FAULTS])

    # ---- markdown ----
    counts = sorted({row["trials"] for row in rows})
    trials = f"{counts[0]}" if len(counts) == 1 else f"{counts[0]} to {counts[-1]}"
    lines = [
        "# Rare-fault sampling experiment",
        "",
        f"{trials} trials per strategy. Same workload every run (`experiments/rare.js`): "
        f"3000 or 3001 requests at a constant arrival rate, with each of the five "
        "faults appearing once in every 1000 requests, so about three chances per trial to catch each one.",
        "",
        "Detection is probabilistic at this fault rate, so each number is a proportion with an "
        "interval around it. The interval treats the trial as the unit, by resampling whole trials, "
        "because the five faults inside a trial share one gateway and one sampler and are not five "
        "independent chances. `off` mirrors nothing and is the cost and latency baseline.",
        "",
    ]
    if invalid:
        lines += [f"Runs excluded as invalid: {', '.join(invalid)}.", ""]

    lines += [
        "## Detection",
        "",
        "| Strategy | Mirrored | Faults found per trial | Detection | 95% CI, trial-clustered |",
        "|---|---|---|---|---|",
    ]
    for row in rows:
        low, high = row["cluster_ci"]
        note = " *" if row["cluster_collapsed"] else ""
        lines.append(
            f"| {row['label']} | {row['mean_mirrored']:.0f} / {row['mean_requests']:.0f} "
            f"({row['mean_actual_rate_pct']:.1f}%) | {row['mean_faults_detected']:.1f} of 5 | "
            f"{row['pooled_probability']*100:.0f}% | {low*100:.0f}-{high*100:.0f}%{note} |"
        )
    lines += [
        "",
        "\\* every trial gave the same answer, so there is no spread to measure and the interval "
        "collapses to a point. That is a limit of ten trials rather than evidence of precision.",
    ]

    lines += [
        "",
        "## Detection probability per fault",
        "",
        "Trials where the fault was caught at all, with the 95% interval.",
        "",
        "| Strategy | " + " | ".join(FAULTS) + " |",
        "|---|" + "---|" * len(FAULTS),
    ]
    for row in rows:
        cells = []
        for fault in FAULTS:
            f = row["per_fault"][fault]
            cells.append(f"{f['found']}/{f['trials']} ({f['probability']*100:.0f}%, "
                         f"{f['ci_low']*100:.0f}-{f['ci_high']*100:.0f})")
        lines.append(f"| {row['label']} | " + " | ".join(cells) + " |")

    lines += [
        "",
        "## Exact requests until first detection",
        "",
        "The exact position the gateway gave the detecting request, counted from its restart, "
        "averaged over the trials where the fault was found. Not an estimate from the request rate.",
        "",
        "These runs recorded when the fault-bearing request *arrived*, not when the worker "
        "*confirmed* the regression. Those are different: the candidate still has to answer, and "
        "R4 deliberately takes the full two second timeout. Confirmation timing is measured "
        "separately in `confirmation-timing.json`.",
        "",
        "| Strategy | " + " | ".join(FAULTS) + " |",
        "|---|" + "---|" * len(FAULTS),
    ]
    for row in rows:
        cells = []
        for fault in FAULTS:
            f = row["per_fault"][fault]
            if f["found"] == 0:
                cells.append("never found")
            else:
                cells.append(f"{fmt(f['mean_requests_until_detection'], 0)} ± "
                             f"{fmt(f['sd_requests_until_detection'], 0)}")
        lines.append(f"| {row['label']} | " + " | ".join(cells) + " |")

    lines += [
        "",
        "## Cost",
        "",
        "CPU is an exact cgroup counter difference over the run. Memory is the largest sample taken "
        "during it, roughly every two seconds.",
        "",
        "| Strategy | Gateway CPU | Candidate CPU | Worker CPU | Relay CPU | Total CPU | CPU per 1000 requests | Gateway peak memory |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for row in rows:
        lines.append(
            f"| {row['label']} | {fmt(row['cpu']['gateway'], 2)} s | {fmt(row['cpu']['candidate-service'], 2)} s | "
            f"{fmt(row['cpu']['comparison-worker'], 2)} s | {fmt(row['cpu']['outbox-relay'], 2)} s | "
            f"{row['total_cpu_seconds']:.2f} s | {fmt(row['cpu_per_1k_requests'], 2)} s | "
            f"{fmt(row['mem']['gateway'], 1)} MB |"
        )

    lines += [
        "",
        "## Client latency through the gateway",
        "",
        "| Strategy | p50 | p95 | p99 |",
        "|---|---|---|---|",
    ]
    for row in rows:
        lines.append(
            f"| {row['label']} | {row['mean_p50_ms']:.2f} ms (sd {row['sd_p50_ms']:.2f}) | "
            f"{row['mean_p95_ms']:.2f} ms (sd {row['sd_p95_ms']:.2f}) | {row['mean_p99_ms']:.2f} ms |"
        )

    lines += [
        "",
        "## Cost per detection",
        "",
        "| Strategy | Mirrored requests per fault caught | CPU seconds per fault caught |",
        "|---|---|---|",
    ]
    for row in rows:
        lines.append(f"| {row['label']} | {fmt(row['mirrored_per_detection'], 0)} | "
                     f"{fmt(row['cpu_per_detection'], 1)} |")

    isolation_ok = sum(r["isolation_ok_trials"] for r in rows)
    isolation_runs = sum(r["trials"] for r in rows)
    lines += [
        "",
        f"Shadow write isolation held in {isolation_ok} of {isolation_runs} runs.",
        "",
    ]
    (out / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")

    print(f"wrote {csv_path} and {out / 'summary.md'}")
    for row in rows:
        low, high = row["pooled_ci"]
        print(f"  {row['label']:<16} {row['mean_actual_rate_pct']:5.1f}% mirrored, "
              f"detection {row['pooled_probability']*100:3.0f}% ({low*100:.0f}-{high*100:.0f}%), "
              f"cpu {row['total_cpu_seconds']:.2f}s")
    if invalid:
        print(f"  excluded {len(invalid)} invalid run(s): {', '.join(invalid)}")


if __name__ == "__main__":
    main()
