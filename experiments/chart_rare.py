"""Chart the rare-fault results.

    python experiments/chart_rare.py            reads results/rare-faults
    python experiments/chart_rare.py out.png

Two panels, because the experiment asks two things: how often does each
strategy catch a rare fault, and what does it cost to run.

The error bars resample whole trials rather than treating all fifty fault
chances as independent, because the five faults inside one trial share a
gateway and a sampler. Where a strategy gave the same answer in every trial
there is no spread to show and the bar has no whiskers; that is ten trials
being a small number, not precision.
"""

import json
import random
import statistics
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint", "risk"]
LABELS = {
    "off": "off\n0%",
    "full": "full\n100%",
    "random-50": "random\n50%",
    "random-25": "random\n25%",
    "random-10": "random\n10%",
    "endpoint": "endpoint\nrules",
    "risk": "risk\npriority",
}
FAULTS = ["R1", "R2", "R3", "R4", "R5"]
CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]


def cluster_interval(proportions, seed=20261026, rounds=20000):
    """Resample whole trials, so the five faults inside one are not counted as
    five independent chances."""
    n = len(proportions)
    mean = statistics.fmean(proportions)
    if n < 2 or statistics.stdev(proportions) == 0:
        return mean, mean
    rng = random.Random(seed)
    means = sorted(statistics.fmean(rng.choices(proportions, k=n)) for _ in range(rounds))
    return means[int(0.025 * rounds)], means[int(0.975 * rounds)]


def main():
    out = Path("results/rare-faults")
    target = Path(sys.argv[1]) if len(sys.argv) > 1 else out / "strategies.png"

    runs = {}
    for path in sorted(out.glob("*.json")):
        if path.name in ("summary.json",) or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if "strategy" in run and run.get("valid", True):
            runs.setdefault(run["strategy"], []).append(run)
    if not runs:
        print(f"no run files in {out}")
        return

    strategies = [s for s in ORDER if s in runs]
    trials = len(runs[strategies[0]])

    probs, lows, highs, rates, cpus = [], [], [], [], []
    for name in strategies:
        found = sum(
            1 for r in runs[name] for f in FAULTS if r["detection"]["per_fault"][f]["detected"]
        )
        total = len(runs[name]) * len(FAULTS)
        per_trial = [r["detection"]["faults_detected"] / len(FAULTS) for r in runs[name]]
        low, high = cluster_interval(per_trial)
        probs.append(100 * found / total)
        lows.append(100 * (found / total) - 100 * low)
        highs.append(100 * high - 100 * (found / total))
        rates.append(statistics.fmean(r["sampling"]["actual_rate_pct"] for r in runs[name]))
        cpus.append(statistics.fmean(
            sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS)
            for r in runs[name]
        ))

    fig, (left, right) = plt.subplots(1, 2, figsize=(13, 5))
    fig.suptitle(
        f"MirrorGate rare-fault experiment, {trials} trials per strategy "
        f"(3000 requests, each fault once in 1000)",
        fontsize=12,
    )

    colors = ["#9aa4b1" if s == "off" else "#4a6fa5" for s in strategies]
    left.bar(range(len(strategies)), probs, yerr=[lows, highs], capsize=5,
             color=colors, width=0.6)
    left.set_xticks(range(len(strategies)))
    # The nominal rates on the labels are within a point of the measured ones,
    # and summary.md carries the exact figures.
    left.set_xticklabels([LABELS[s] for s in strategies])
    left.set_ylabel("faults caught (% of chances)")
    left.set_ylim(0, 115)
    left.set_title("Detection, 95% intervals clustered by trial")
    # The label goes inside a tall bar and above the error bar on a short one,
    # otherwise the two collide at the bottom of the scale.
    for i, p in enumerate(probs):
        inside = p > 20
        left.text(i, p / 2 if inside else p + highs[i] + 4, f"{p:.0f}%", ha="center",
                  color="white" if inside else "#333", fontsize=10, fontweight="bold")
    left.grid(axis="y", alpha=0.25)
    left.set_axisbelow(True)

    right.bar(range(len(strategies)), cpus, color=colors, width=0.6)
    right.set_xticks(range(len(strategies)))
    right.set_xticklabels([LABELS[s] for s in strategies])
    right.set_ylabel("CPU seconds per run, all containers")
    right.set_title("Cost: measured CPU, exact cgroup counters")
    for i, c in enumerate(cpus):
        right.text(i, c + max(cpus) * 0.02, f"{c:.1f}s", ha="center", fontsize=9)
    right.set_ylim(0, max(cpus) * 1.18)
    right.grid(axis="y", alpha=0.25)
    right.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(target, dpi=150)
    print(f"wrote {target}")


if __name__ == "__main__":
    main()
