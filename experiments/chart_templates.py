"""Chart what route templates do to the sampler's view.

    python experiments/chart_templates.py            reads results/pr4/templates
    python experiments/chart_templates.py out.png

Left is the bookkeeping: twenty endpoints become five, because every product
id folds into one route.

Right is the consequence. Scored per url, each fault-bearing id is a tenth of
a percent of traffic on its own, which makes it rare, which lifts it to 25%.
Grouped, those requests belong to a route carrying three quarters of the
traffic, which is not rare at all, so they sit at the 10% floor until
something actually goes wrong on that route. R3 and R5 are unaffected,
because their endpoints were never a product id.

Both panels are the state at the end of one trial. Risk rates move during a
run, so this is a snapshot of where they ended up, not a constant.
"""

import json
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ARMS = [
    ("risk", "grouped by route", "#2f6fb0"),
    ("risk-urls", "scored per URL", "#c2663f"),
]
BANDS = [10, 25, 50, 100]
# Each fault, and the endpoint it is scored under: per url, then templated.
FAULT_ENDPOINTS = [
    ("R1", ("GET /api/products/12", "GET /api/products/{id}")),
    ("R2", ("GET /api/products/8", "GET /api/products/{id}")),
    ("R3", ("GET /api/products", "GET /api/products")),
    ("R4", ("GET /api/users/7", "GET /api/users/{id}")),
    ("R5", ("POST /api/orders", "POST /api/orders")),
]
INK = "#2b2b2b"


def main():
    out = Path("results/pr4/templates")
    target = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("results/pr4/templates-rates.png")

    counts, fault_rates, totals = {}, {}, {}
    for name, _, _ in ARMS:
        path = out / f"{name}-t1.json"
        if not path.exists():
            print(f"no {path}")
            return 1
        endpoints = json.loads(path.read_text(encoding="utf-8"))["sampling"]["endpoints"]
        totals[name] = len(endpoints)
        counts[name] = [sum(1 for e in endpoints if round(e["rate"] * 100) == band)
                        for band in BANDS]
        by_key = {e["endpoint"]: e["rate"] * 100 for e in endpoints}
        templated = name == "risk"
        fault_rates[name] = [by_key.get(key[templated], 0) for _, key in FAULT_ENDPOINTS]

    fig, (left, right) = plt.subplots(1, 2, figsize=(13, 5))
    fig.suptitle("What risk-priority was scoring at the end of one trial, "
                 "with and without route templates", fontsize=12)

    width = 0.36
    positions = range(len(BANDS))
    for i, (name, label, color) in enumerate(ARMS):
        offset = (i - 0.5) * (width + 0.02)
        bars = [p + offset for p in positions]
        left.bar(bars, counts[name], width=width, color=color,
                 label=f"{label} ({totals[name]} endpoints)")
        for x, value in zip(bars, counts[name]):
            if value:
                left.text(x, value + 0.4, str(value), ha="center", fontsize=9, color=INK)
        fault_bars = [i + offset for i in range(len(FAULT_ENDPOINTS))]
        right.bar(fault_bars, fault_rates[name], width=width, color=color, label=label)
        for x, value in zip(fault_bars, fault_rates[name]):
            right.text(x, value + 2.5, f"{value:.0f}%", ha="center", fontsize=9, color=INK)

    left.set_xticks(list(positions))
    left.set_xticklabels([f"{band}%" for band in BANDS])
    left.set_xlabel("sampling rate risk-priority gave the endpoint")
    left.set_ylabel("endpoints")
    left.set_title("Endpoints being scored")
    top = max(max(counts.values()))
    left.set_ylim(0, top * 1.25)
    left.set_yticks(range(0, top + 1, 5 if top > 10 else 2))

    right.set_xticks(range(len(FAULT_ENDPOINTS)))
    # The urls are long enough to collide, and /api/ is on all of them.
    right.set_xticklabels([fault + "\n" + key[0].split(" ")[1].replace("/api/", "")
                           for fault, key in FAULT_ENDPOINTS])
    right.set_xlabel("the endpoint each seeded fault lives on")
    right.set_ylabel("sampling rate at the end of the run")
    right.set_title("What the fault-bearing endpoints were getting")
    right.set_ylim(0, 118)
    right.set_yticks([0, 10, 25, 50, 100])

    for axis in (left, right):
        axis.grid(axis="y", alpha=0.25)
        axis.set_axisbelow(True)
    left.legend(frameon=False, loc="upper right")
    right.legend(frameon=False, loc="upper left")

    fig.tight_layout()
    fig.savefig(target, dpi=150)
    print(f"wrote {target}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
