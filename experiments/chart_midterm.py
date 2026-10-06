"""Chart the midterm results for the report.

    python experiments/chart_midterm.py            reads results/midterm
    python experiments/chart_midterm.py results/midterm/strategies.png

Two panels, because the experiment is really asking two questions: what does
each strategy cost, and how fast does it notice. Everything plotted here comes
out of the run files; nothing is smoothed or filled in.
"""

import json
import statistics
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ORDER = ["full", "random-50", "random-25", "random-10", "endpoint", "risk"]
LABELS = {
    "full": "full\n100%",
    "random-50": "random\n50%",
    "random-25": "random\n25%",
    "random-10": "random\n10%",
    "endpoint": "endpoint\nrules",
    "risk": "risk\npriority",
}
REGRESSIONS = ["R1", "R2", "R3", "R4", "R5"]
REGRESSION_LABELS = {
    "R1": "R1 price",
    "R2": "R2 500",
    "R3": "R3 slow",
    "R4": "R4 timeout",
    "R5": "R5 order total",
}


def load(out):
    runs = {}
    for path in sorted(out.glob("*.json")):
        if path.name in ("summary.json",) or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        runs.setdefault(run["strategy"], []).append(run)
    return runs


def main():
    out = Path("results/midterm")
    target = Path(sys.argv[1]) if len(sys.argv) > 1 else out / "strategies.png"
    runs = load(out)
    if not runs:
        print(f"no run files in {out}")
        return

    strategies = [s for s in ORDER if s in runs]
    trials = len(runs[strategies[0]])

    rates = [statistics.fmean(r["sampling"]["actual_rate_pct"] for r in runs[s]) for s in strategies]
    found = [statistics.fmean(r["detection"]["regressions_detected"] for r in runs[s]) for s in strategies]

    fig, (left, right) = plt.subplots(1, 2, figsize=(13, 5))
    fig.suptitle(
        f"MirrorGate sampling strategies, mean of {trials} trials "
        f"(3000 requests each, 5 known regressions)",
        fontsize=12,
    )

    # Panel 1: what it costs against what it finds.
    bars = left.bar(range(len(strategies)), rates, color="#4a6fa5", width=0.6)
    left.set_xticks(range(len(strategies)))
    left.set_xticklabels([LABELS[s] for s in strategies])
    left.set_ylabel("traffic mirrored (%)")
    left.set_ylim(0, 125)
    left.set_title("Cost: how much traffic was mirrored")
    # Both labels go above the bar: a 10% bar has no room inside it.
    for bar, rate, f in zip(bars, rates, found):
        left.text(bar.get_x() + bar.get_width() / 2, rate + 3,
                  f"{rate:.0f}%\n{f:.1f}/5 found", ha="center", fontsize=10)
    left.grid(axis="y", alpha=0.25)
    left.set_axisbelow(True)

    # Panel 2: how much traffic each strategy needed before it noticed.
    width = 0.8 / len(strategies)
    colors = plt.get_cmap("viridis")([i / max(len(strategies) - 1, 1) for i in range(len(strategies))])
    for i, s in enumerate(strategies):
        values, misses = [], []
        for rid in REGRESSIONS:
            per = [r["detection"]["per_regression"][rid] for r in runs[s]]
            got = [p["requests_until_detection"] for p in per if p["requests_until_detection"] is not None]
            values.append(statistics.fmean(got) if got else 0)
            misses.append(len(got) < len(per))
        offsets = [x + i * width - 0.4 + width / 2 for x in range(len(REGRESSIONS))]
        right.bar(offsets, values, width=width, label=LABELS[s].replace("\n", " "), color=colors[i])
        # Mark the ones that were not found in every trial, and label the
        # zeroes so an instant detection doesn't look like missing data.
        for x, value, missed in zip(offsets, values, misses):
            if missed:
                right.text(x, value + 20, "x", ha="center", color="#b3261e", fontsize=11, fontweight="bold")
            elif value < 1:
                right.text(x, 8, "0", ha="center", fontsize=7, color="#333")

    right.set_xticks(range(len(REGRESSIONS)))
    right.set_xticklabels([REGRESSION_LABELS[r] for r in REGRESSIONS])
    right.set_ylabel("requests until first detection")
    right.set_title("Speed: traffic needed before the fault was caught")
    right.legend(fontsize=8, ncol=2)
    right.grid(axis="y", alpha=0.25)
    right.set_axisbelow(True)
    right.text(0.99, 0.97, "x = missed in at least one trial", transform=right.transAxes,
               ha="right", va="top", fontsize=8, color="#b3261e")
    right.text(0.99, 0.92, "0 = caught on the first request of the run", transform=right.transAxes,
               ha="right", va="top", fontsize=8, color="#333")

    fig.tight_layout()
    fig.savefig(target, dpi=150)
    print(f"wrote {target}")


if __name__ == "__main__":
    main()
