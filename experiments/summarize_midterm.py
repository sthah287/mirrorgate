"""Average the midterm trials into summary.csv and summary.md.

    python experiments/summarize_midterm.py results/midterm

Every number here is a mean over the trials that actually ran, with the
standard deviation where it is worth having. Nothing is filled in or smoothed:
if a strategy missed a regression in two runs out of five, the detection rate
is 60%.
"""

import csv
import json
import statistics
import sys
from pathlib import Path

ORDER = ["full", "random-50", "random-25", "random-10", "endpoint", "risk"]
REGRESSIONS = ["R1", "R2", "R3", "R4", "R5"]

LABELS = {
    "full": "full (100%)",
    "random-50": "random 50%",
    "random-25": "random 25%",
    "random-10": "random 10%",
    "endpoint": "endpoint rules",
    "risk": "risk-priority",
}


def mean(values):
    return statistics.fmean(values) if values else None


def stdev(values):
    return statistics.stdev(values) if len(values) > 1 else 0.0


def fmt(value, places=1):
    return "-" if value is None else f"{value:.{places}f}"


def main():
    out = Path(sys.argv[1] if len(sys.argv) > 1 else "results/midterm")
    runs = [json.loads(p.read_text(encoding="utf-8")) for p in sorted(out.glob("*.json"))
            if not p.name.endswith((".k6.json", ".stats.json", ".db.json", ".sampling.json", ".external.json"))
            and p.name != "summary.json"]
    if not runs:
        print(f"no run files in {out}")
        return

    by_strategy = {}
    for run in runs:
        by_strategy.setdefault(run["strategy"], []).append(run)

    rows = []
    for name in ORDER + sorted(set(by_strategy) - set(ORDER)):
        trials = by_strategy.get(name)
        if not trials:
            continue
        trials.sort(key=lambda r: r["trial"])

        requests = [t["workload"]["total_requests"] for t in trials]
        mirrored = [t["sampling"]["mirrored"] for t in trials]
        actual = [t["sampling"]["actual_rate_pct"] for t in trials]
        stored = [t["stored"]["total"] for t in trials]
        found = [t["detection"]["regressions_detected"] for t in trials]
        p50 = [t["latency_ms"]["p50"] for t in trials]
        p95 = [t["latency_ms"]["p95"] for t in trials]
        p99 = [t["latency_ms"]["p99"] for t in trials]
        isolation = [t["side_effects"]["isolation_ok"] for t in trials]

        # Per regression: how often it was found at all, and how long it took
        # in the trials where it was found.
        per_regression = {}
        for rid in REGRESSIONS:
            hits = [t["detection"]["per_regression"][rid] for t in trials]
            times = [h["first_detection_s"] for h in hits if h["first_detection_s"] is not None]
            reqs = [h["requests_until_detection"] for h in hits if h["requests_until_detection"] is not None]
            per_regression[rid] = {
                "found_in_trials": sum(1 for h in hits if h["detected"]),
                "trials": len(hits),
                "mean_first_detection_s": mean(times),
                "sd_first_detection_s": stdev(times),
                "mean_requests_until_detection": mean(reqs),
                "sd_requests_until_detection": stdev(reqs),
            }

        all_times = [v["mean_first_detection_s"] for v in per_regression.values()
                     if v["mean_first_detection_s"] is not None]

        rows.append({
            "strategy": name,
            "label": LABELS.get(name, name),
            "trials": len(trials),
            "mean_requests": mean(requests),
            "mean_mirrored": mean(mirrored),
            "mean_actual_rate_pct": mean(actual),
            "sd_actual_rate_pct": stdev(actual),
            "mean_stored": mean(stored),
            "mean_regressions_detected": mean(found),
            "detection_rate_pct": 100 * sum(found) / (len(trials) * len(REGRESSIONS)),
            "mean_p50_ms": mean(p50),
            "sd_p50_ms": stdev(p50),
            "mean_p95_ms": mean(p95),
            "sd_p95_ms": stdev(p95),
            "mean_p99_ms": mean(p99),
            "slowest_mean_detection_s": max(all_times) if all_times else None,
            "isolation_ok_trials": sum(1 for ok in isolation if ok),
            "per_regression": per_regression,
        })

    csv_path = out / "summary.csv"
    with csv_path.open("w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow([
            "strategy", "trials", "mean_requests", "mean_mirrored", "mean_actual_rate_pct",
            "sd_actual_rate_pct", "mean_stored", "mean_regressions_detected", "detection_rate_pct",
            "mean_p50_ms", "sd_p50_ms", "mean_p95_ms", "sd_p95_ms", "mean_p99_ms",
        ] + [f"{r}_found_in_trials" for r in REGRESSIONS]
          + [f"{r}_mean_detection_s" for r in REGRESSIONS]
          + [f"{r}_mean_requests_until_detection" for r in REGRESSIONS])
        for row in rows:
            writer.writerow([
                row["strategy"], row["trials"], f"{row['mean_requests']:.0f}", f"{row['mean_mirrored']:.0f}",
                f"{row['mean_actual_rate_pct']:.2f}", f"{row['sd_actual_rate_pct']:.2f}",
                f"{row['mean_stored']:.0f}", f"{row['mean_regressions_detected']:.2f}",
                f"{row['detection_rate_pct']:.1f}", f"{row['mean_p50_ms']:.3f}", f"{row['sd_p50_ms']:.3f}",
                f"{row['mean_p95_ms']:.3f}", f"{row['sd_p95_ms']:.3f}", f"{row['mean_p99_ms']:.3f}",
            ] + [row["per_regression"][r]["found_in_trials"] for r in REGRESSIONS]
              + [fmt(row["per_regression"][r]["mean_first_detection_s"], 2) for r in REGRESSIONS]
              + [fmt(row["per_regression"][r]["mean_requests_until_detection"], 0) for r in REGRESSIONS])

    # Normally every strategy has the same number of trials; say so honestly
    # if a run was interrupted partway through.
    counts = sorted({row["trials"] for row in rows})
    trials = f"{counts[0]}" if len(counts) == 1 else f"{counts[0]} to {counts[-1]}"
    lines = [
        "# Midterm sampling experiment",
        "",
        f"{trials} trials per strategy, same workload (`experiments/midterm.js`) every time: "
        f"{rows[0]['mean_requests']:.0f} requests at a constant arrival rate, five known regressions "
        "(R1-R5 in `docs/regression-catalog.md`). Means across trials.",
        "",
        "## Cost and detection",
        "",
        "| Strategy | Mirrored | Actual rate | Stored | Regressions found | Detection rate |",
        "|---|---|---|---|---|---|",
    ]
    for row in rows:
        lines.append(
            f"| {row['label']} | {row['mean_mirrored']:.0f} / {row['mean_requests']:.0f} | "
            f"{row['mean_actual_rate_pct']:.1f}% (sd {row['sd_actual_rate_pct']:.1f}) | "
            f"{row['mean_stored']:.0f} | {row['mean_regressions_detected']:.1f} of 5 | "
            f"{row['detection_rate_pct']:.0f}% |"
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

    isolation_total = sum(r["isolation_ok_trials"] for r in rows)
    isolation_runs = sum(r["trials"] for r in rows)
    lines += [
        "",
        f"Shadow write isolation held in {isolation_total} of {isolation_runs} runs: one live "
        "payment per order the client sent, and sandbox payments only for mirrored orders.",
        "",
        "## Per regression",
        "",
        "Trials where it was found, and the mean request count to the first detection in those "
        "trials, plus or minus the standard deviation across them. The spread matters as much as "
        "the mean: where two strategies overlap inside it, they are not distinguishable here.",
        "",
        "| Strategy | " + " | ".join(REGRESSIONS) + " |",
        "|---|" + "---|" * len(REGRESSIONS),
    ]
    for row in rows:
        cells = []
        for rid in REGRESSIONS:
            r = row["per_regression"][rid]
            if r["found_in_trials"] == 0:
                cells.append("never found")
            else:
                cells.append(
                    f"{r['found_in_trials']}/{r['trials']}, "
                    f"~{fmt(r['mean_requests_until_detection'], 0)} "
                    f"± {fmt(r['sd_requests_until_detection'], 0)} req"
                )
        lines.append(f"| {row['label']} | " + " | ".join(cells) + " |")

    lines.append("")
    (out / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")

    print(f"wrote {csv_path} and {out / 'summary.md'}")
    for row in rows:
        print(
            f"  {row['label']:<16} {row['mean_actual_rate_pct']:5.1f}% mirrored, "
            f"{row['mean_regressions_detected']:.1f}/5 regressions, p95 {row['mean_p95_ms']:.2f}ms"
        )


if __name__ == "__main__":
    main()
