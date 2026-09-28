"""Turn the per-strategy result files into summary.csv and a printed table.

    python experiments/summarize.py results/progress-report-2
"""

import csv
import json
import sys
from pathlib import Path

ORDER = ["off", "full", "random-50", "random-25", "random-10", "endpoint", "endpoint-fixed"]

COLUMNS = [
    "strategy",
    "mode",
    "target_rate",
    "requests",
    "test_duration_s",
    "eligible",
    "mirrored",
    "actual_rate",
    "comparisons_stored",
    "regressions_detected",
    "regressions_missed",
    "p50_ms",
    "p95_ms",
    "p99_ms",
    "throughput_rps",
]


def row(result):
    detected = result["regressions"]["detected"]
    return {
        "strategy": result["strategy"],
        "mode": result["sampling"]["mode"],
        "target_rate": result["sampling"]["target_rate"],
        "requests": result["traffic"]["total_requests"],
        "test_duration_s": result["traffic"]["test_duration_s"],
        "eligible": result["mirroring"]["eligible"],
        "mirrored": result["mirroring"]["mirrored"],
        "actual_rate": f"{result['mirroring']['actual_rate'] * 100:.1f}%",
        "comparisons_stored": result["mirroring"]["comparisons_stored"],
        "regressions_detected": f"{sum(1 for v in detected.values() if v > 0)}/4",
        "regressions_missed": len(result["regressions"]["missed"]),
        "p50_ms": result["client_latency_ms"]["p50"],
        "p95_ms": result["client_latency_ms"]["p95"],
        "p99_ms": result["client_latency_ms"]["p99"],
        "throughput_rps": result["traffic"]["throughput_rps"],
    }


def main():
    out_dir = Path(sys.argv[1])
    files = {p.stem: p for p in out_dir.glob("*.json") if "." not in p.stem}

    rows = []
    for name in ORDER:
        if name in files:
            rows.append(row(json.loads(files[name].read_text())))
    for name in sorted(set(files) - set(ORDER)):
        rows.append(row(json.loads(files[name].read_text())))

    if not rows:
        print("no results found in", out_dir)
        return

    csv_path = out_dir / "summary.csv"
    with csv_path.open("w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=COLUMNS)
        writer.writeheader()
        writer.writerows(rows)

    widths = {c: max(len(c), max(len(str(r[c])) for r in rows)) for c in COLUMNS}
    print()
    print("  ".join(c.ljust(widths[c]) for c in COLUMNS))
    print("  ".join("-" * widths[c] for c in COLUMNS))
    for r in rows:
        print("  ".join(str(r[c]).ljust(widths[c]) for c in COLUMNS))
    print()
    print("wrote", csv_path)


if __name__ == "__main__":
    main()
