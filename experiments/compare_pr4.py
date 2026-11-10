"""The comparisons the week 12 experiment was run to make.

    python experiments/compare_pr4.py [results/pr4/rare-random]

Two questions, both paired by trial because trial n of one arm and trial n of
the other saw the same fault positions and ran back to back:

  risk-priority against random 10%   - the week 9 question, with twice the
                                       trials and the positions randomised
  risk-priority against itself with  - the week 11 question: does grouping
  route templates turned off           urls into routes help or hurt

Every per-fault test here is one of five on the same data, so a nominal p
value is reported next to a Bonferroni threshold rather than on its own.
Nothing here reads a summary file.
"""

import json
import statistics
import sys
from collections import defaultdict
from pathlib import Path

from stats import cluster_bootstrap, cluster_t, mcnemar_exact, paired_t, wilson

FAULTS = ["R1", "R2", "R3", "R4", "R5"]
ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint",
         "risk", "risk-urls"]
# Risk-priority picks its own rate, so "which caught more" only means
# something next to how much traffic each arm mirrored to do it. The first
# two pairs are roughly rate matched, the third is the week 11 question, and
# the fourth is the comparison the week 9 report made, kept for continuity
# even though the two arms no longer mirror the same share.
PAIRS = [
    ("risk-urls", "random-10"),
    ("risk", "random-25"),
    ("risk", "risk-urls"),
    ("risk", "random-10"),
]
CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]


def load(directory):
    runs = defaultdict(list)
    for path in sorted(directory.glob("*.json")):
        if path.name.count(".") > 1 or path.name in ("summary.json", "comparisons.json"):
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if "strategy" in run and run.get("valid", True):
            runs[run["strategy"]].append(run)
    for arm in runs.values():
        arm.sort(key=lambda r: r["trial"])
    return runs


def caught_share(run):
    return run["detection"]["faults_detected"] / len(FAULTS)


def detection_table(runs):
    print("=" * 78)
    print("DETECTION, TRIAL AS THE UNIT")
    print("=" * 78)
    print(f"  {'strategy':<12} {'caught':>8} {'mirrored':>9} {'rate':>7}"
          f"   {'clustered t':>15} {'bootstrap':>15}   pooled Wilson")
    summary = {}
    for name in ORDER:
        arm = runs.get(name)
        if not arm:
            continue
        per_trial = [caught_share(r) for r in arm]
        caught = sum(r["detection"]["faults_detected"] for r in arm)
        total = len(arm) * len(FAULTS)
        mean, low, high = cluster_t(per_trial)
        blow, bhigh = cluster_bootstrap(per_trial)
        wlow, whigh = wilson(caught, total)
        mirrored = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in arm)
        summary[name] = {
            "trials": len(arm),
            "per_trial_proportions": per_trial,
            "detected": caught,
            "opportunities": total,
            "detection": mean,
            "mirrored_pct": mirrored,
            "cluster_t": [low, high],
            "cluster_bootstrap": [blow, bhigh],
            "pooled_wilson": [wlow, whigh],
            "degenerate": low is not None and low == high,
        }
        t_txt = f"{low*100:4.0f}-{high*100:3.0f}%" if low is not None else "     n/a"
        if summary[name]["degenerate"]:
            t_txt += "*"
        print(f"  {name:<12} {caught:>4}/{total:<3} {mirrored:>8.1f}% {mean*100:>6.0f}%"
              f"   {t_txt:>15} {blow*100:>8.0f}-{bhigh*100:3.0f}%   {wlow*100:>5.0f}-{whigh*100:3.0f}%")
    print()
    print("  * every trial gave the same answer, so the spread is zero and the")
    print("    interval collapses to a point. That is the trial count, not precision.")
    print("  The pooled Wilson column assumes the five faults in a trial are five")
    print("  independent chances. They are not; it is here to show how much")
    print("  narrower that assumption makes the answer look.")
    return summary


def compare(runs, a, b):
    print("\n" + "=" * 78)
    print(f"{a.upper()} AGAINST {b.upper()}, PAIRED BY TRIAL")
    print("=" * 78)
    if a not in runs or b not in runs:
        print("  one of the arms is missing")
        return None

    arm_a, arm_b = runs[a], runs[b]
    trials = min(len(arm_a), len(arm_b))
    differences = [caught_share(arm_a[i]) - caught_share(arm_b[i]) for i in range(trials)]
    mean, low, high = paired_t(differences)
    ahead = sum(1 for d in differences if d > 0)
    behind = sum(1 for d in differences if d < 0)

    rate_a = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in arm_a)
    rate_b = statistics.fmean(r["sampling"]["actual_rate_pct"] for r in arm_b)
    matched = abs(rate_a - rate_b) <= 5
    print(f"  mirrored: {rate_a:.1f}% against {rate_b:.1f}%"
          f"{'' if matched else '  <- not rate matched, read the cost table too'}")
    print(f"  per-trial difference in share of faults caught: {mean*100:+.0f} points")
    if low is not None:
        print(f"  paired 95% interval: {low*100:+.0f} to {high*100:+.0f} points")
    print(f"  {a} ahead in {ahead} trials, behind in {behind}, level in "
          f"{trials - ahead - behind}")
    separated = low is not None and (low > 0 or high < 0)
    print(f"  -> {'separated' if separated else 'not separated'} at {trials} trials")

    print("\n  per fault, paired, with a Bonferroni threshold of 0.05/5 = 0.010:")
    print(f"  {'fault':<6} {a:>12} {b:>12} {'only ' + a:>14} {'only ' + b:>14} {'p':>8}")
    per_fault = {}
    for fault in FAULTS:
        hits_a = [r["detection"]["per_fault"][fault]["detected"] for r in arm_a[:trials]]
        hits_b = [r["detection"]["per_fault"][fault]["detected"] for r in arm_b[:trials]]
        only_a = sum(1 for x, y in zip(hits_a, hits_b) if x and not y)
        only_b = sum(1 for x, y in zip(hits_a, hits_b) if y and not x)
        p = mcnemar_exact(only_a, only_b)
        per_fault[fault] = {
            "a_detected": sum(hits_a), "b_detected": sum(hits_b),
            "only_a": only_a, "only_b": only_b, "p": p,
            "meets_bonferroni": p < 0.01,
        }
        flag = " *" if p < 0.01 else ""
        print(f"  {fault:<6} {sum(hits_a):>10}/{trials} {sum(hits_b):>10}/{trials} "
              f"{only_a:>14} {only_b:>14} {p:>8.4f}{flag}")
    print("  * meets the adjusted threshold. Anything that does not is exploratory,")
    print("    whatever its nominal p value says.")

    return {
        "trials": trials,
        "mirrored_pct": [rate_a, rate_b],
        "rate_matched": matched,
        "per_trial_differences": differences,
        "mean_difference": mean,
        "paired_interval": [low, high],
        "separated": separated,
        "trials_ahead": ahead,
        "trials_behind": behind,
        "per_fault": per_fault,
    }


def efficiency(runs):
    print("\n" + "=" * 78)
    print("WHAT A DETECTION COST")
    print("=" * 78)
    print(f"  {'strategy':<12} {'mirrored per fault caught':>26} {'CPU seconds per fault':>23}")
    out = {}
    for name in ORDER:
        arm = runs.get(name)
        if not arm:
            continue
        caught = sum(r["detection"]["faults_detected"] for r in arm)
        if caught == 0:
            print(f"  {name:<12} {'nothing caught':>26} {'':>23}")
            out[name] = None
            continue
        mirrored = sum(r["sampling"]["mirrored"] for r in arm)
        cpu = sum(sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0
                      for c in CONTAINERS) for r in arm)
        out[name] = {"mirrored_per_fault": mirrored / caught, "cpu_per_fault": cpu / caught}
        print(f"  {name:<12} {mirrored / caught:>26.0f} {cpu / caught:>23.2f}")
    return out


def main():
    directory = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("results/pr4/rare-random")
    runs = load(directory)
    if not runs:
        print(f"no run files in {directory}")
        return 1
    trials = {name: len(arm) for name, arm in runs.items()}
    print(f"reading {sum(trials.values())} runs from {directory}")
    print(f"trials per arm: {trials}\n")

    summary = detection_table(runs)
    comparisons = {f"{a}_vs_{b}": compare(runs, a, b) for a, b in PAIRS}
    costs = efficiency(runs)

    out = directory / "comparisons.json"
    out.write_text(json.dumps({
        "method": "the trial is the unit of observation; paired comparisons use the "
                  "trial pairing, which is real because both arms of a trial saw the "
                  "same fault positions; per-fault McNemar is exact and two sided, "
                  "and five faults means five tests on one dataset",
        "strategies": summary,
        "comparisons": comparisons,
        "cost_per_detection": costs,
    }, indent=1) + "\n", encoding="utf-8")
    print(f"\nwrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
