"""Independent check of the randomised-position experiment, from the raw files.

    python experiments/verify_pr4.py [results/pr4/rare-random]

Nothing here reads summary.csv or summary.md. Everything is recomputed from
the per-run JSON, so the summaries can be checked rather than trusted.

What it checks:
  1. the randomisation did what it claims: one seed per trial, the same fault
     positions for every strategy inside a trial, different positions between
     trials, and three occurrences of each fault
  2. the plan and the run agree, so a detection can be tied to the occurrence
     that produced it
  3. nothing was lost: mirrored against stored, per run
  4. detection percentages, recomputed, with intervals that treat the trial
     as the unit
  5. which occurrence of a fault was the one caught
  6. whether the cost numbers are comparable between arms
"""

import json
import statistics
import sys
from collections import defaultdict
from pathlib import Path

from stats import cluster_bootstrap

FAULTS = ["R1", "R2", "R3", "R4", "R5"]
ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint",
         "risk", "risk-urls"]
CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]


def load(directory):
    runs = defaultdict(list)
    for path in sorted(directory.glob("*.json")):
        if path.name.count(".") > 1 or path.name == "summary.json":
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        if "strategy" in run:
            runs[run["strategy"]].append(run)
    for arm in runs.values():
        arm.sort(key=lambda r: r["trial"])
    return runs


def check_randomisation(runs, problems):
    print("1. randomisation")
    by_trial = defaultdict(dict)
    for strategy, arm in runs.items():
        for run in arm:
            plan = run.get("fault_plan")
            if not plan:
                problems.append(f"{run['run']} has no fault plan")
                continue
            positions = {f: tuple(plan["faults"][f]["expected_request_seq"]) for f in FAULTS}
            by_trial[run["trial"]][strategy] = (plan["seed"], positions)

    shared, differ = 0, 0
    for trial, arms in sorted(by_trial.items()):
        seeds = {seed for seed, _ in arms.values()}
        layouts = {tuple(sorted(p.items())) for _, p in arms.values()}
        if len(seeds) != 1 or len(layouts) != 1:
            problems.append(f"trial {trial} did not use one layout across its arms")
        else:
            shared += 1

    layouts = {}
    for trial, arms in sorted(by_trial.items()):
        seed, positions = next(iter(arms.values()))
        key = tuple(sorted(positions.items()))
        if key in layouts:
            problems.append(f"trials {layouts[key]} and {trial} drew the same positions")
        else:
            layouts[key] = trial
            differ += 1

    counts = {len(p) for _, arms in by_trial.items()
              for _, positions in arms.values() for p in positions.values()}
    print(f"   trials where every arm used one seed and one layout: {shared} of {len(by_trial)}")
    print(f"   trials with a layout no other trial had:              {differ} of {len(by_trial)}")
    print(f"   occurrences per fault:                                {sorted(counts)}")
    if counts != {3}:
        problems.append(f"expected three occurrences per fault, saw {sorted(counts)}")

    example = sorted(by_trial)[:3]
    for trial in example:
        seed, positions = next(iter(by_trial[trial].values()))
        shown = ", ".join(f"{f}={list(positions[f])}" for f in FAULTS[:2])
        print(f"   trial {trial}: seed {seed}, {shown}, ...")


def check_plan_matches_run(runs, problems):
    print("\n2. the plan and the run agree")
    off_plan = [run["run"] for arm in runs.values() for run in arm
                if run.get("fault_plan") and run["fault_plan"]["detections_off_plan"]]
    total = sum(len(arm) for arm in runs.values())
    drifts = [run["fault_plan"]["max_position_drift"] for arm in runs.values()
              for run in arm if run.get("fault_plan")]
    exact = sum(1 for d in drifts if d == 0)
    print(f"   runs where every detection was at its exact planned position: {exact} of {total}")
    print(f"   largest gap between a detection and its planned position:     "
          f"{max(drifts, default=0)} requests")
    print(f"   runs with a detection too far away to match at all:           {len(off_plan)} of {total}")
    if off_plan:
        problems.append(f"detections off the plan in {off_plan[:5]}")

    full = runs.get("full", [])
    caught = sum(len(run["fault_plan"]["faults"][f]["caught_request_seq"])
                 for run in full for f in FAULTS)
    planned = sum(len(run["fault_plan"]["faults"][f]["expected_request_seq"])
                  for run in full for f in FAULTS)
    print(f"   under full sampling, occurrences caught: {caught} of {planned} planned")
    if full and caught != planned:
        problems.append(f"full sampling missed {planned - caught} planned occurrences")


def check_no_loss(runs, problems):
    print("\n3. event loss")
    mismatched, failures, pending = [], 0, 0
    for arm in runs.values():
        for run in arm:
            if run["sampling"]["mirrored"] != run["stored"]["total"]:
                mismatched.append(run["run"])
            failures += run["sampling"]["publish_failed"]
            pending += run["outbox"].get("pending", 0)
    total = sum(len(arm) for arm in runs.values())
    print(f"   runs where mirrored != stored: {len(mismatched)} of {total}")
    print(f"   publish failures, all runs:    {failures}")
    print(f"   outbox rows left pending:      {pending}")
    if mismatched:
        problems.append(f"mirrored != stored in {mismatched[:5]}")


def check_detection(runs):
    print("\n4. detection, recomputed")
    print(f"   {'strategy':<12} {'mirrored':>9} {'detection':>10} {'95% CI (trial-clustered)':>26}")
    rows = {}
    for strategy in ORDER:
        arm = runs.get(strategy)
        if not arm:
            continue
        per_trial = [sum(1 for f in FAULTS if run["detection"]["per_fault"][f]["detected"]) / len(FAULTS)
                     for run in arm]
        rate = statistics.fmean(run["sampling"]["actual_rate_pct"] for run in arm)
        low, high = cluster_bootstrap(per_trial)
        found = statistics.fmean(per_trial)
        rows[strategy] = (found, low, high, rate, len(arm))
        print(f"   {strategy:<12} {rate:>8.1f}% {found*100:>9.0f}% "
              f"{low*100:>14.0f}% to {high*100:>3.0f}%")
    return rows


def check_occurrences(runs):
    print("\n5. which occurrence was caught")
    print("   of the faults a strategy caught, the occurrence that caught it")
    print(f"   {'strategy':<12} {'1st':>5} {'2nd':>5} {'3rd':>5} {'median requests after the first':>33}")
    for strategy in ORDER:
        arm = runs.get(strategy)
        if not arm:
            continue
        which = {1: 0, 2: 0, 3: 0}
        gaps = []
        for run in arm:
            plan = run.get("fault_plan")
            if not plan:
                continue
            for fault in FAULTS:
                index = plan["faults"][fault]["first_caught_occurrence"]
                if index in which:
                    which[index] += 1
                gap = plan["faults"][fault]["requests_after_first_occurrence"]
                if gap is not None:
                    gaps.append(gap)
        median = f"{statistics.median(gaps):.0f}" if gaps else "n/a"
        print(f"   {strategy:<12} {which[1]:>5} {which[2]:>5} {which[3]:>5} {median:>33}")


def check_cost(runs):
    print("\n6. cost")
    print(f"   {'strategy':<12} {'total CPU':>10} {'per 1000 requests':>19} {'gateway peak MB':>17}")
    for strategy in ORDER:
        arm = runs.get(strategy)
        if not arm:
            continue
        cpus, peaks, per_k = [], [], []
        for run in arm:
            total = sum((run["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS)
            cpus.append(total)
            per_k.append(1000 * total / run["workload"]["total_requests"])
            peak = (run["resources"].get("gateway") or {}).get("mem_peak_mb")
            if peak:
                peaks.append(peak)
        print(f"   {strategy:<12} {statistics.fmean(cpus):>9.2f}s {statistics.fmean(per_k):>18.2f}s "
              f"{statistics.fmean(peaks) if peaks else 0:>16.1f}")
    missing = [run["run"] for arm in runs.values() for run in arm
               for c in CONTAINERS if (run["resources"].get(c) or {}).get("cpu_seconds") is None]
    print(f"   runs with a container whose CPU was not readable: {len(missing)}")
    return missing


def main():
    directory = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("results/pr4/rare-random")
    runs = load(directory)
    if not runs:
        print(f"no run files in {directory}")
        return 1
    print(f"reading {sum(len(a) for a in runs.values())} run files from {directory}\n")

    problems = []
    check_randomisation(runs, problems)
    check_plan_matches_run(runs, problems)
    check_no_loss(runs, problems)
    check_detection(runs)
    check_occurrences(runs)
    missing = check_cost(runs)
    if missing:
        problems.append(f"{len(missing)} runs are missing a container's CPU")

    print()
    if problems:
        print("PROBLEMS")
        for problem in problems:
            print(f"  - {problem}")
        return 1
    print("every check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
