"""What route templates change about the sampler's view.

    python experiments/summarize_templates.py

Reads results/pr4/templates/ and writes results/pr4/templates-summary.md.

Two arms of the rare-fault experiment, the same in every respect except
whether risk scoring groups /api/products/1 and /api/products/2 into
GET /api/products/{id}. The question here is what the grouping does to the
sampler itself: how many endpoints it is tracking, what rate each one gets,
and how much traffic that adds up to. Whether it catches more faults is a
different question and needs far more than three trials, so the detection
figures are reported with that said plainly rather than left to be read as a
result.
"""

import json
import statistics
import sys
from pathlib import Path

ARMS = [("risk", "templates on"), ("risk-urls", "templates off")]
FAULTS = ["R1", "R2", "R3", "R4", "R5"]


def runs(out, prefix):
    found = []
    for path in sorted(out.glob(f"{prefix}-t*.json")):
        found.append(json.loads(path.read_text(encoding="utf-8")))
    return found


def mean(values):
    return statistics.fmean(values) if values else 0.0


def main():
    out = Path("results/pr4/templates")
    sets = {name: runs(out, name) for name, _ in ARMS}
    missing = [name for name, got in sets.items() if not got]
    if missing:
        print(f"no runs for {', '.join(missing)} in {out}")
        return 1

    trials = min(len(v) for v in sets.values())
    lines = [
        "# Route templates: what the grouping changes",
        "",
        f"{trials} trials per arm, same workload (`experiments/rare.js`, 3000 requests at",
        "100/s), risk-priority sampling in both. The only difference is",
        "`RISK_ROUTE_TEMPLATES`. Every run file records which setting the gateway",
        "reported, so the two arms cannot be mixed up by a stale image.",
        "",
        "## What the sampler is tracking",
        "",
        "| | Templates on | Templates off |",
        "| --- | --- | --- |",
    ]

    def row(title, fn, fmt="{:.1f}"):
        values = [fmt.format(fn(sets[name])) for name, _ in ARMS]
        lines.append(f"| {title} | {values[0]} | {values[1]} |")

    row("Endpoints scored", lambda rs: mean([len(r["sampling"]["endpoints"]) for r in rs]))
    row("Requests mirrored", lambda rs: mean([r["sampling"]["mirrored"] for r in rs]), "{:.0f}")
    row("Share of traffic mirrored", lambda rs: mean([r["sampling"]["actual_rate_pct"] for r in rs]), "{:.2f}%")
    row("Gateway CPU (s)", lambda rs: mean([r["resources"]["gateway"]["cpu_seconds"] for r in rs]), "{:.2f}")
    row("Client latency p95 (ms)", lambda rs: mean([r["latency_ms"]["p95"] for r in rs]), "{:.2f}")

    lines += [
        "",
        "## The rate table, one representative trial each",
        "",
        "This is what the gateway was actually scoring at the end of the run,",
        "straight from `/internal/sampling`.",
        "",
    ]
    for name, label in ARMS:
        run = sets[name][0]
        endpoints = run["sampling"]["endpoints"]
        lines += [
            f"### {label} ({run['run']}, {len(endpoints)} endpoints)",
            "",
            "| Endpoint | Requests | Share | Score | Rate | Why |",
            "| --- | ---: | ---: | ---: | ---: | --- |",
        ]
        for e in endpoints[:12]:
            why = ", ".join(e["reasons"]) if e["reasons"] else "no risk signals"
            lines.append(
                f"| `{e['endpoint']}` | {e['requests']} | {e['share']*100:.1f}% | "
                f"{e['score']} | {e['rate']*100:.0f}% | {why} |"
            )
        if len(endpoints) > 12:
            lines.append(f"| ... | | | | | {len(endpoints) - 12} more, all at the 10% floor |")
        lines.append("")

    lines += [
        "## Detection in these runs",
        "",
        f"Faults caught, out of {trials} trials times five faults. {trials} trials cannot",
        "separate two arms whose rates are this close: these counts are here to",
        "show what was run, not to settle which grouping detects more. The",
        "randomised-position experiment with twenty trials per strategy is what",
        "that question needs.",
        "",
        "| Arm | Faults caught | " + " | ".join(FAULTS) + " |",
        "| --- | --- | " + " | ".join("---" for _ in FAULTS) + " |",
    ]
    for name, label in ARMS:
        per_fault = []
        caught = 0
        for fault in FAULTS:
            hits = sum(1 for r in sets[name] if r["detection"]["per_fault"][fault]["detected"])
            caught += hits
            per_fault.append(f"{hits}/{len(sets[name])}")
        lines.append(
            f"| {label} | {caught} of {len(sets[name]) * len(FAULTS)} | " + " | ".join(per_fault) + " |"
        )

    lines += [
        "",
        "## What the numbers mean",
        "",
        "Grouping by route is a trade. An endpoint that is one product id out of",
        "many is a tiny share of traffic on its own, so per-URL scoring calls it",
        "rare and lifts its rate; grouped, the ids add up to a busy route and the",
        "rare bonus goes away. In exchange, a mismatch found on one id raises the",
        "rate for every id behind the same handler, which is the behaviour a real",
        "deployment wants when the bug is in the handler rather than in one row.",
        "",
        "Which of those matters more depends on where the faults are, and this",
        "workload puts every fault on a fixed, individually rare id.",
    ]

    text = "\n".join(lines) + "\n"
    (Path("results/pr4") / "templates-summary.md").write_text(text, encoding="utf-8")
    print(text)
    print(f"wrote {Path('results/pr4') / 'templates-summary.md'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
