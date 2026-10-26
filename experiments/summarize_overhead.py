"""Compare mirroring off against mirroring everything.

    python experiments/summarize_overhead.py results/overhead

The question is whether mirroring costs the user anything measurable. Each arm
runs several times, so the answer is a difference with an interval around it
rather than two single numbers.

The interval is Welch's t interval for the difference of two means: the two
arms have their own spread and only a handful of trials each, so a normal
approximation would be too confident. If the interval contains zero, this
experiment did not find a difference, and that is what gets written.
"""

import json
import math
import statistics
import sys
from pathlib import Path

CONTAINERS = ["gateway", "candidate-service", "comparison-worker", "outbox-relay", "postgres"]
# Welch with a handful of trials per arm lands near 8 degrees of freedom.
T_95 = 2.306


def load_runs(out):
    runs = {}
    for path in sorted(out.glob("*.json")):
        if path.name in ("summary.json", "slow-threshold.json") or path.name.count(".") > 1:
            continue
        run = json.loads(path.read_text(encoding="utf-8"))
        runs.setdefault(run["arm"], []).append(run)
    return runs


def welch(a, b):
    """Difference of means (b - a) and its 95% interval."""
    if len(a) < 2 or len(b) < 2:
        return statistics.fmean(b) - statistics.fmean(a), None, None
    mean_a, mean_b = statistics.fmean(a), statistics.fmean(b)
    var_a, var_b = statistics.variance(a), statistics.variance(b)
    se = math.sqrt(var_a / len(a) + var_b / len(b))
    diff = mean_b - mean_a
    return diff, diff - T_95 * se, diff + T_95 * se


def describe(diff, low, high, unit):
    if low is None:
        return f"{diff:+.3f} {unit}"
    verdict = "no measurable difference" if low <= 0 <= high else "measurable"
    return f"{diff:+.3f} {unit} (95% CI {low:+.3f} to {high:+.3f}, {verdict})"


def main():
    out = Path(sys.argv[1] if len(sys.argv) > 1 else "results/overhead")
    runs = load_runs(out)
    if "off" not in runs or "full" not in runs:
        print(f"need both arms in {out}, found {sorted(runs)}")
        return

    off, full = runs["off"], runs["full"]
    trials = min(len(off), len(full))
    rate = off[0]["rate_per_s"]
    requests = statistics.fmean([r["total_requests"] for r in off + full])

    lines = [
        "# Mirroring overhead",
        "",
        f"{len(off)} trials with mirroring off and {len(full)} with everything mirrored, "
        f"at {rate:.0f} requests per second, about {requests:.0f} requests per run. Same workload, "
        "same container limits, same reset between runs.",
        "",
        "## Client latency",
        "",
        "| | off | full | difference |",
        "|---|---|---|---|",
    ]

    summary = {"trials": trials, "rate_per_s": rate, "latency": {}, "resources": {}}
    for metric in ("p50", "p95", "p99"):
        a = [r["latency_ms"][metric] for r in off]
        b = [r["latency_ms"][metric] for r in full]
        diff, low, high = welch(a, b)
        lines.append(
            f"| {metric} | {statistics.fmean(a):.3f} ms (sd {statistics.stdev(a):.3f}) | "
            f"{statistics.fmean(b):.3f} ms (sd {statistics.stdev(b):.3f}) | {describe(diff, low, high, 'ms')} |"
        )
        summary["latency"][metric] = {
            "off_mean": statistics.fmean(a), "full_mean": statistics.fmean(b),
            "diff": diff, "ci_low": low, "ci_high": high,
        }

    lines += [
        "",
        "## CPU, exact cgroup counter differences",
        "",
        "| Container | off | full | difference |",
        "|---|---|---|---|",
    ]
    for container in CONTAINERS:
        a = [(r["resources"].get(container) or {}).get("cpu_seconds") or 0 for r in off]
        b = [(r["resources"].get(container) or {}).get("cpu_seconds") or 0 for r in full]
        diff, low, high = welch(a, b)
        lines.append(
            f"| {container} | {statistics.fmean(a):.2f} s | {statistics.fmean(b):.2f} s | "
            f"{describe(diff, low, high, 's')} |"
        )
        summary["resources"][container] = {
            "off_mean_cpu_s": statistics.fmean(a), "full_mean_cpu_s": statistics.fmean(b),
            "diff": diff, "ci_low": low, "ci_high": high,
        }

    total_off = statistics.fmean([sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS) for r in off])
    total_full = statistics.fmean([sum((r["resources"].get(c) or {}).get("cpu_seconds") or 0 for c in CONTAINERS) for r in full])
    lines += [
        "",
        f"Total measured CPU: {total_off:.2f} s with mirroring off, {total_full:.2f} s with everything "
        f"mirrored, so mirroring cost {total_full - total_off:+.2f} CPU seconds "
        f"({100 * (total_full - total_off) / total_off:+.0f}%) for {requests:.0f} requests.",
        "",
        "## Peak memory, largest sample during the run",
        "",
        "| Container | off | full |",
        "|---|---|---|",
    ]
    for container in CONTAINERS:
        a = [(r["resources"].get(container) or {}).get("mem_peak_mb") or 0 for r in off]
        b = [(r["resources"].get(container) or {}).get("mem_peak_mb") or 0 for r in full]
        lines.append(f"| {container} | {statistics.fmean(a):.1f} MB | {statistics.fmean(b):.1f} MB |")

    dropped = sum(r["skipped_in_flight"] for r in full)
    if dropped:
        lines += ["", f"Note: {dropped} shadow requests were skipped because the in-flight limit was "
                      "full, so at this rate the gateway was already shedding some mirroring work."]

    lines.append("")
    (out / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")

    print(f"wrote {out / 'summary.md'}")
    for metric in ("p50", "p95", "p99"):
        s = summary["latency"][metric]
        print(f"  {metric}: off {s['off_mean']:.3f}ms, full {s['full_mean']:.3f}ms, "
              f"diff {s['diff']:+.3f}ms")
    print(f"  total CPU: off {total_off:.2f}s, full {total_full:.2f}s")


if __name__ == "__main__":
    main()
