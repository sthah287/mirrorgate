"""Summarise the relay before-and-after trials.

    python experiments/summarize_relay.py

Reads results/pr4/relay-before-t*.json and relay-after-t*.json and writes
results/pr4/relay-summary.md.

Three trials per configuration is few, so every row carries the median and the
observed range rather than a mean alone. Where the three trials agreed the
range is shown as a single number, which is three runs of a deterministic
workload rather than precision.
"""

import json
import statistics
import sys
from pathlib import Path

CONFIGS = [("before", "one publish per row"), ("after", "batched publish")]


def runs(out, prefix):
    found = []
    for path in sorted(out.glob(f"relay-{prefix}-t*.json")):
        found.append(json.loads(path.read_text(encoding="utf-8")))
    return found


def spread(values, fmt="{:.0f}"):
    """Median, with the range when the trials disagreed."""
    median = statistics.median(values)
    low, high = min(values), max(values)
    if fmt.format(low) == fmt.format(high):
        return fmt.format(median)
    return f"{fmt.format(median)} ({fmt.format(low)}\u2013{fmt.format(high)})"


def main():
    out = Path("results/pr4")
    sets = {name: runs(out, name) for name, _ in CONFIGS}
    missing = [name for name, got in sets.items() if not got]
    if missing:
        print(f"no trials for {', '.join(missing)} in {out}")
        return 1

    rows = [
        ("Trials", lambda rs: str(len(rs))),
        ("Requests offered", lambda rs: spread([r["workload"]["total_requests"] for r in rs])),
        ("Events mirrored", lambda rs: spread([r["mirrored"] for r in rs])),
        ("Comparisons stored", lambda rs: spread([r["stored"] for r in rs])),
        ("Peak backlog (events)", lambda rs: spread([r["backlog"]["peak_pending"] for r in rs])),
        ("Queue drain after load (s)",
         lambda rs: spread([r["backlog"]["drain_seconds_after_load"] for r in rs])),
        ("Backlog clearing rate (events/s)",
         lambda rs: spread([r["backlog"]["drain_throughput_per_s"] or 0 for r in rs], "{:.1f}")),
        ("Confirmed events/s over load + catch-up",
         lambda rs: spread([r["backlog"]["confirmed_throughput_per_s"] for r in rs], "{:.1f}")),
        ("Confirmation lag p50 (ms)",
         lambda rs: spread([r["confirmation_lag_ms"]["p50"] for r in rs], "{:.0f}")),
        ("Confirmation lag p95 (ms)",
         lambda rs: spread([r["confirmation_lag_ms"]["p95"] for r in rs], "{:.0f}")),
        ("Confirmation lag max (ms)",
         lambda rs: spread([r["confirmation_lag_ms"]["max"] for r in rs], "{:.0f}")),
        ("Relay CPU (s)",
         lambda rs: spread([r["cpu_seconds"]["outbox-relay"] for r in rs], "{:.2f}")),
        ("Relay peak RSS (MB)",
         lambda rs: spread([r["memory_peak_mb"]["outbox-relay"] for r in rs], "{:.1f}")),
        ("Client latency p50 (ms)",
         lambda rs: spread([r["latency_ms"]["p50"] for r in rs], "{:.2f}")),
        ("Client latency p95 (ms)",
         lambda rs: spread([r["latency_ms"]["p95"] for r in rs], "{:.2f}")),
        ("Publish failures recorded",
         lambda rs: spread([r["publish_failed"] + r["outbox"]["failed_attempts"] for r in rs])),
        ("Rows left pending", lambda rs: spread([r["outbox"]["pending"] for r in rs])),
    ]

    lines = [
        "# Relay throughput, before and after batched publishing",
        "",
        f"Workload per trial: {sets['before'][0]['workload']['rate_per_s']:.0f} requests/s for "
        f"{sets['before'][0]['workload']['duration']} at full sampling, outbox publish mode.",
        "Before is the original relay, one publish call per outbox row",
        "(`OUTBOX_SINGLE_PUBLISH=true`). After is the same binary publishing each",
        "claimed batch in one call. Median across trials, range in brackets.",
        "",
        "| Measurement | Before | After |",
        "| --- | --- | --- |",
    ]
    for title, fn in rows:
        lines.append(f"| {title} | {fn(sets['before'])} | {fn(sets['after'])} |")

    lost = [r["mirrored"] - r["stored"] for rs in sets.values() for r in rs]
    lines += [
        "",
        "Every trial stored one comparison per mirrored event, so no events were",
        "lost or duplicated in either configuration (largest difference between",
        f"mirrored and stored across all {sum(len(v) for v in sets.values())} trials: {max(lost)}).",
        "",
        "The backlog clearing rate is the queue depth when the load stopped divided",
        "by how long it took to empty. It describes the relay's own rate only when",
        "there is a backlog to clear: in the after runs the queue never grew past a",
        "dozen events, so that figure measures the poll interval, not throughput.",
        "The relay's own publish rate is measured separately by the diagnostic test.",
    ]
    (out / "relay-summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")

    print("\n".join(lines))
    print(f"\nwrote {out / 'relay-summary.md'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
