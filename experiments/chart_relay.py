"""Chart the relay before-and-after trials.

    python experiments/chart_relay.py            reads results/pr4
    python experiments/chart_relay.py out.png

Left: the outbox queue depth through each run, all three trials of each
configuration drawn, so the spread between trials is visible rather than
averaged away. Right: how long a request waited between arriving at the
gateway and its comparison being stored.

The left panel is linear, where the after runs sit almost on the axis. That is
the result rather than a drawing problem, so its peak is labelled instead of
being lifted onto a log scale. The right panel is three orders of magnitude
apart end to end, which no linear bar chart can show, so it marks each value
with a dot on a log axis: the position carries the number and no bar length
implies a ratio it cannot support.
"""

import json
import statistics
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

CONFIGS = [
    ("before", "one publish per row", "#c2663f"),
    ("after", "batched publish", "#2f6fb0"),
]
INK = "#2b2b2b"
MUTED = "#6b6b6b"


def runs(out, prefix):
    return [json.loads(p.read_text(encoding="utf-8"))
            for p in sorted(out.glob(f"relay-{prefix}-t*.json"))]


def main():
    out = Path("results/pr4")
    target = Path(sys.argv[1]) if len(sys.argv) > 1 else out / "relay-throughput.png"
    sets = {name: runs(out, name) for name, _, _ in CONFIGS}
    if not all(sets.values()):
        print(f"missing before or after trials in {out}")
        return 1

    trials = min(len(v) for v in sets.values())
    load_seconds = 30
    fig, (left, right) = plt.subplots(1, 2, figsize=(13, 5))
    fig.suptitle(
        f"MirrorGate relay, {trials} trials per configuration "
        f"(3000 requests at 100/s, full sampling)",
        fontsize=12,
    )

    for name, label, color in CONFIGS:
        for i, run in enumerate(sets[name]):
            curve = run["depth_curve"]
            left.plot([d["t"] for d in curve], [d["pending"] for d in curve],
                      color=color, linewidth=2, alpha=0.85,
                      label=label if i == 0 else None)
    peak_before = max(r["backlog"]["peak_pending"] for r in sets["before"])
    peak_after = max(r["backlog"]["peak_pending"] for r in sets["after"])
    left.set_ylim(-peak_before * 0.04, peak_before * 1.16)
    left.axvline(load_seconds, color=MUTED, linestyle="--", linewidth=1)
    left.text(load_seconds + 2, peak_before * 1.12, "load stops",
              color=MUTED, fontsize=9, va="top")
    left.annotate(f"{peak_after} events peak",
                  xy=(load_seconds * 0.75, peak_after),
                  xytext=(load_seconds * 1.4, peak_before * 0.16),
                  color=CONFIGS[1][2], fontsize=9, fontweight="bold",
                  arrowprops=dict(arrowstyle="-", color=CONFIGS[1][2], linewidth=0.8))
    left.set_xlabel("seconds since the run started")
    left.set_ylabel("comparison events waiting in the outbox")
    left.set_title("Backlog through the run")
    left.legend(frameon=False, loc="upper right", bbox_to_anchor=(1.0, 0.88))
    left.grid(alpha=0.25)
    left.set_axisbelow(True)

    # Percentiles share one unit, so they share one axis. Seconds rather than
    # milliseconds because the before figures run past two minutes.
    metrics = [("p50", "median"), ("p95", "95th percentile"), ("max", "slowest")]
    points = {
        name: [[r["confirmation_lag_ms"][key] / 1000 for r in sets[name]]
               for key, _ in metrics]
        for name, _, _ in CONFIGS
    }
    for row in range(len(metrics)):
        right.plot([statistics.median(points["after"][row]),
                    statistics.median(points["before"][row])],
                   [row, row], color=MUTED, linewidth=1, zorder=1)
    for name, label, color in CONFIGS:
        medians = [statistics.median(v) for v in points[name]]
        right.errorbar(medians, range(len(metrics)), fmt="o", color=color,
                       markersize=9, markeredgecolor="white", markeredgewidth=1.5,
                       label=label, zorder=2,
                       xerr=[[m - min(v) for m, v in zip(medians, points[name])],
                             [max(v) - m for m, v in zip(medians, points[name])]],
                       ecolor=color, elinewidth=1, capsize=3)
        # The faster series sits left of the connector, so its label goes above
        # the dot rather than across the line.
        lift = -0.17 if name == "after" else 0
        for row, median in enumerate(medians):
            text = f"{median * 1000:.0f} ms" if median < 10 else f"{median:.0f} s"
            right.text(median * 1.3, row + lift, text, va="center", fontsize=9, color=INK)

    right.set_xscale("log")
    right.set_xlim(0.04, 600)
    right.set_xticks([0.1, 1, 10, 100])
    right.set_xticklabels(["0.1", "1", "10", "100"])
    right.set_yticks(range(len(metrics)))
    right.set_yticklabels([label for _, label in metrics])
    right.set_ylim(len(metrics) - 0.25, -0.55)
    right.set_xlabel("seconds from request arrival to the comparison being stored, log scale")
    right.set_title("Confirmation delay, median of trials with the observed range")
    right.legend(frameon=False, loc="lower center", ncol=2)
    right.grid(axis="x", alpha=0.25)
    right.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(target, dpi=150)
    print(f"wrote {target}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
