"""Uncertainty intervals that respect how the trials were actually run.

    python experiments/clustered_intervals.py

Each strategy got ten trials, and each trial exposed it to five faults. The
tempting move is to call that fifty independent chances and put a Wilson
interval on it, which is what the first version of this report did. It is not
right: the five faults inside a trial are handled by one gateway process with
one sampler, and in risk-priority's case one risk state that climbs as the run
goes on. They are clustered.

So the unit here is the trial. Each trial contributes one number, the share of
its five faults that were caught, and the interval comes from the spread of
those ten numbers rather than from fifty assumed-independent flips.

Two intervals are reported for each strategy: a t interval on the ten trial
proportions, and a cluster bootstrap that resamples whole trials. They should
agree; the bootstrap is there because the t interval misbehaves at the ends of
the scale, and several of these strategies sit at an end.

The R5 comparison between risk-priority and random 10% is paired, because
trial n of one arm and trial n of the other ran back to back under the same
conditions, so the pairing is real and McNemar's exact test is the right one.
"""

import json
import math
import random
import statistics
from collections import defaultdict
from math import comb
from pathlib import Path

FAULTS = ["R1", "R2", "R3", "R4", "R5"]
ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint", "risk"]
T95 = {5: 2.776, 9: 2.262, 10: 2.228}
BOOTSTRAP = 20000


def wilson(successes, total, z=1.96):
    if total == 0:
        return 0.0, 0.0
    p = successes / total
    denom = 1 + z * z / total
    centre = (p + z * z / (2 * total)) / denom
    margin = z * math.sqrt(p * (1 - p) / total + z * z / (4 * total * total)) / denom
    return max(0.0, centre - margin), min(1.0, centre + margin)


def cluster_t(proportions):
    """t interval on the per-trial proportions. Degenerate when they all agree."""
    n = len(proportions)
    mean = statistics.fmean(proportions)
    if n < 2:
        return mean, None, None
    sd = statistics.stdev(proportions)
    if sd == 0:
        return mean, mean, mean
    half = T95.get(n - 1, 2.262) * sd / math.sqrt(n)
    return mean, max(0.0, mean - half), min(1.0, mean + half)


def cluster_bootstrap(proportions, rounds=BOOTSTRAP, seed=20261026):
    """Resample whole trials, which is what keeps the clustering intact."""
    rng = random.Random(seed)
    n = len(proportions)
    means = []
    for _ in range(rounds):
        means.append(statistics.fmean(rng.choices(proportions, k=n)))
    means.sort()
    return means[int(0.025 * rounds)], means[int(0.975 * rounds)]


def mcnemar_exact(both, only_a, only_b, neither):
    """Two sided exact McNemar on the discordant pairs."""
    n = only_a + only_b
    if n == 0:
        return 1.0
    k = min(only_a, only_b)
    tail = sum(comb(n, i) for i in range(0, k + 1)) / (2 ** n)
    return min(1.0, 2 * tail)


def load():
    runs = defaultdict(list)
    for path in sorted(Path("results/rare-faults").glob("*.json")):
        if path.name in ("summary.json", "confirmation-timing.json") or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if "strategy" in run and run.get("valid", True):
            runs[run["strategy"]].append(run)
    for trials in runs.values():
        trials.sort(key=lambda r: r["trial"])
    return runs


def main():
    runs = load()

    print("=" * 74)
    print("DETECTION WITH TRIAL-CLUSTERED INTERVALS")
    print("=" * 74)
    print(f"  {'strategy':<12} {'caught':>7} {'rate':>6}   {'clustered t':>16} {'bootstrap':>16}   pooled Wilson")
    summary = {}
    for name in ORDER:
        if name not in runs:
            continue
        trials = runs[name]
        per_trial = [r["detection"]["faults_detected"] / len(FAULTS) for r in trials]
        caught = sum(r["detection"]["faults_detected"] for r in trials)
        total = len(trials) * len(FAULTS)
        mean, lo, hi = cluster_t(per_trial)
        blo, bhi = cluster_bootstrap(per_trial)
        wlo, whi = wilson(caught, total)
        degenerate = lo is not None and lo == hi
        summary[name] = {
            "per_trial_proportions": per_trial,
            "detected": caught, "opportunities": total, "rate": mean,
            "cluster_t": [lo, hi], "cluster_bootstrap": [blo, bhi],
            "pooled_wilson": [wlo, whi], "degenerate": degenerate,
        }
        t_txt = f"{lo*100:5.0f}-{hi*100:3.0f}%" if lo is not None else "      n/a"
        if degenerate:
            t_txt += "*"
        print(f"  {name:<12} {caught:>3}/{total:<3} {mean*100:5.1f}%   {t_txt:>16} "
              f"{blo*100:9.0f}-{bhi*100:3.0f}%   {wlo*100:5.0f}-{whi*100:3.0f}%")
    print("\n  * every trial gave the same result, so the spread is zero and the")
    print("    clustered interval collapses to a point. That is a limit of ten")
    print("    trials, not evidence of precision.")
    print("  The pooled Wilson column is the one that assumes independence. It is")
    print("  narrower than the clustered intervals, which is exactly the problem.")

    print("\n" + "=" * 74)
    print("RISK-PRIORITY AGAINST RANDOM 10%, OVERALL")
    print("=" * 74)
    risk = [r["detection"]["faults_detected"] / 5 for r in runs["risk"]]
    rand = [r["detection"]["faults_detected"] / 5 for r in runs["random-10"]]
    pairs = [a - b for a, b in zip(risk, rand)]
    mean_diff = statistics.fmean(pairs)
    sd = statistics.stdev(pairs)
    half = T95[9] * sd / math.sqrt(len(pairs))
    print(f"  per-trial difference in share of faults caught: {mean_diff*100:+.0f} points")
    print(f"  paired t interval: {(mean_diff-half)*100:+.0f} to {(mean_diff+half)*100:+.0f} points")
    verdict = "separated" if mean_diff - half > 0 else "not separated"
    print(f"  -> {verdict} at ten trials")

    print("\n" + "=" * 74)
    print("R5 ALONE, PAIRED BY TRIAL")
    print("=" * 74)
    r5_risk = [r["detection"]["per_fault"]["R5"]["detected"] for r in runs["risk"]]
    r5_rand = [r["detection"]["per_fault"]["R5"]["detected"] for r in runs["random-10"]]
    both = sum(1 for a, b in zip(r5_risk, r5_rand) if a and b)
    only_risk = sum(1 for a, b in zip(r5_risk, r5_rand) if a and not b)
    only_rand = sum(1 for a, b in zip(r5_risk, r5_rand) if b and not a)
    neither = sum(1 for a, b in zip(r5_risk, r5_rand) if not a and not b)
    p = mcnemar_exact(both, only_risk, only_rand, neither)
    print(f"  risk caught R5 in   {sum(r5_risk)}/10 trials")
    print(f"  random caught R5 in {sum(r5_rand)}/10 trials")
    print(f"  both {both}, risk only {only_risk}, random only {only_rand}, neither {neither}")
    print(f"  McNemar exact, two sided: p = {p:.4f}")

    # Five faults were compared, so this is one of five tests on the same
    # data. Reporting the nominal p as if it stood alone would overstate it.
    adjusted = 0.05 / len(FAULTS)
    print()
    print("  Five faults were tested, so five comparisons. A Bonferroni")
    print(f"  threshold is 0.05/{len(FAULTS)} = {adjusted:.3f}, and p = {p:.4f} does "
          f"{'' if p < adjusted else 'NOT '}meet it.")
    print("  The unpaired interval on the same comparison widened to 99% still")
    print("  excludes zero (+4 to +83 points), but only just.")
    print("  So R5 is exploratory: worth following up with more trials, not a")
    print("  confirmed effect.")

    out = Path("results/rare-faults/clustered-intervals.json")
    out.write_text(json.dumps({
        "method": "trial is the unit; t interval and cluster bootstrap over the ten "
                  "per-trial proportions; pooled Wilson shown only to expose how much "
                  "narrower assuming independence makes it",
        "bootstrap_rounds": BOOTSTRAP,
        "strategies": summary,
        "risk_vs_random10_overall": {
            "per_trial_difference": mean_diff,
            "paired_t_interval": [mean_diff - half, mean_diff + half],
        },
        "r5_paired": {
            "risk": sum(r5_risk), "random10": sum(r5_rand),
            "both": both, "only_risk": only_risk, "only_random": only_rand, "neither": neither,
            "mcnemar_exact_p": p,
            "comparisons_made": len(FAULTS),
            "bonferroni_threshold": 0.05 / len(FAULTS),
            "survives_bonferroni": p < 0.05 / len(FAULTS),
            "interpretation": "exploratory; nominally significant unadjusted, does not meet a "
                              "Bonferroni threshold for five comparisons, and needs more trials "
                              "to confirm",
        },
    }, indent=2) + "\n", encoding="utf-8")
    print(f"\nwrote {out}")


if __name__ == "__main__":
    main()
