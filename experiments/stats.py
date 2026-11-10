"""The statistics the experiment scripts share.

Each of these was written inside whichever script needed it first and then
needed again by the next one. The week 12 scripts import them from here
instead of carrying another copy. `verify_results.py` and
`clustered_intervals.py` still have their own: they are the record of what
produced the week 9 numbers and are left exactly as they ran.

Nothing in here is clever. The reason it is written down rather than taken
from a library is that the choice between these intervals is the part of the
analysis that matters, and the comments are where that choice is argued.
"""

import math
import random
import statistics

# Two sided 95% t, indexed by degrees of freedom. Only the sizes the
# experiments actually use are listed; anything else falls back to 1.96,
# which is the large-sample limit.
T95 = {
    1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365,
    8: 2.306, 9: 2.262, 10: 2.228, 11: 2.201, 12: 2.179, 13: 2.160,
    14: 2.145, 15: 2.131, 16: 2.120, 17: 2.110, 18: 2.101, 19: 2.093,
    20: 2.086, 24: 2.064, 29: 2.045,
}


def t95(df):
    return T95.get(df, 1.96)


def wilson(successes, total, z=1.96):
    """Interval for a proportion that behaves near 0 and 1.

    The normal approximation runs off the ends of the scale at these sample
    sizes, which is exactly where several of these results sit.
    """
    if total == 0:
        return 0.0, 0.0
    p = successes / total
    denom = 1 + z * z / total
    centre = (p + z * z / (2 * total)) / denom
    margin = z * math.sqrt(p * (1 - p) / total + z * z / (4 * total * total)) / denom
    return max(0.0, centre - margin), min(1.0, centre + margin)


def cluster_t(proportions):
    """Mean and t interval over per-trial proportions.

    The unit is the trial, not the fault. The faults inside one trial share a
    gateway process, a sampler, and in risk mode a risk state that climbs as
    the run goes on, so they are not independent chances.
    """
    n = len(proportions)
    mean = statistics.fmean(proportions)
    if n < 2:
        return mean, None, None
    sd = statistics.stdev(proportions)
    if sd == 0:
        return mean, mean, mean
    half = t95(n - 1) * sd / math.sqrt(n)
    return mean, max(0.0, mean - half), min(1.0, mean + half)


def cluster_bootstrap(proportions, rounds=20000, seed=20261026):
    """The same interval by resampling whole trials.

    Reported beside the t interval because the t interval misbehaves at the
    ends of the scale and several strategies sit at an end.
    """
    n = len(proportions)
    if n == 0:
        return 0.0, 0.0
    if n < 2 or statistics.pstdev(proportions) == 0:
        return proportions[0], proportions[0]
    rng = random.Random(seed)
    means = sorted(statistics.fmean(rng.choices(proportions, k=n)) for _ in range(rounds))
    return means[int(0.025 * rounds)], means[int(0.975 * rounds)]


def paired_t(differences):
    """Mean difference and its 95% interval, for arms run trial by trial.

    Trial n of one arm and trial n of the other ran back to back under the
    same conditions, so the pairing is real and throwing it away would waste
    most of the precision.
    """
    n = len(differences)
    mean = statistics.fmean(differences)
    if n < 2:
        return mean, None, None
    sd = statistics.stdev(differences)
    if sd == 0:
        return mean, mean, mean
    half = t95(n - 1) * sd / math.sqrt(n)
    return mean, mean - half, mean + half


def mcnemar_exact(only_a, only_b):
    """Two-sided exact McNemar on the discordant pairs.

    Trials where both arms caught the fault, or neither did, carry no
    information about which is better; only the disagreements do.
    """
    n = only_a + only_b
    if n == 0:
        return 1.0
    k = min(only_a, only_b)
    tail = sum(math.comb(n, i) for i in range(0, k + 1)) / (2 ** n)
    return min(1.0, 2 * tail)
