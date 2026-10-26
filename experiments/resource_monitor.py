"""Measure what a run actually costs in CPU and memory.

    python experiments/resource_monitor.py out.json stopfile

Runs until `stopfile` appears, then writes a JSON summary.

Two different methods, because the two numbers are not equally trustworthy:

CPU is exact. cgroup v1 exposes cpuacct.usage, a cumulative nanosecond
counter per container, so the difference between the start and the end of the
run is precisely the CPU time that container used during it. Nothing is
sampled or averaged.

Memory is sampled. This machine runs cgroup v1, where the peak counter is
memory.max_usage_in_bytes, and it is cumulative since the container started.
It cannot be reset between runs here (the cgroup mount inside the container is
read only), so using it would report the largest value since boot rather than
the largest during this run. Instead memory.usage_in_bytes is sampled through
`docker stats` for the length of the run and the peak is the largest sample.
One `docker stats` call for all containers takes about two seconds, so the
sampling interval is roughly that, and a spike shorter than the gap between
two samples can be missed.
"""

import json
import re
import subprocess
import sys
import time
from pathlib import Path

CONTAINERS = {
    "gateway": "mirrorgate-gateway-1",
    "candidate-service": "mirrorgate-candidate-service-1",
    "stable-service": "mirrorgate-stable-service-1",
    "comparison-worker": "mirrorgate-comparison-worker-1",
    "outbox-relay": "mirrorgate-outbox-relay-1",
    "postgres": "mirrorgate-postgres-1",
}

UNITS = {"b": 1, "kib": 1024, "mib": 1024**2, "gib": 1024**3, "kb": 1000, "mb": 1000**2, "gb": 1000**3}


def cpu_nanoseconds(container):
    """Exact cumulative CPU time for one container, or None if unreadable."""
    try:
        out = subprocess.run(
            ["docker", "exec", container, "cat", "/sys/fs/cgroup/cpuacct/cpuacct.usage"],
            capture_output=True, text=True, timeout=20,
        )
        if out.returncode == 0:
            return int(out.stdout.strip())
    except (subprocess.SubprocessError, ValueError):
        pass
    return None


def sample_memory():
    """One `docker stats` call covering every container being watched."""
    try:
        out = subprocess.run(
            ["docker", "stats", "--no-stream", "--format", "{{.Name}}|{{.MemUsage}}", *CONTAINERS.values()],
            capture_output=True, text=True, timeout=30,
        )
    except subprocess.SubprocessError:
        return {}
    if out.returncode != 0:
        return {}

    by_name = {v: k for k, v in CONTAINERS.items()}
    samples = {}
    for line in out.stdout.splitlines():
        # docker stats paints the terminal, so strip any escape sequences.
        line = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", line).strip()
        if "|" not in line:
            continue
        name, usage = line.split("|", 1)
        label = by_name.get(name.strip())
        if not label:
            continue
        match = re.match(r"\s*([\d.]+)\s*([A-Za-z]+)", usage)
        if match:
            value, unit = float(match.group(1)), match.group(2).lower()
            samples[label] = value * UNITS.get(unit, 1)
    return samples


def main():
    out_path = Path(sys.argv[1])
    stop_path = Path(sys.argv[2])

    start_cpu = {label: cpu_nanoseconds(name) for label, name in CONTAINERS.items()}
    started = time.time()
    memory = {label: [] for label in CONTAINERS}

    while not stop_path.exists():
        for label, value in sample_memory().items():
            memory[label].append(value)
        time.sleep(0.2)

    window = time.time() - started
    end_cpu = {label: cpu_nanoseconds(name) for label, name in CONTAINERS.items()}

    containers = {}
    for label in CONTAINERS:
        before, after = start_cpu[label], end_cpu[label]
        cpu_seconds = None
        if before is not None and after is not None and after >= before:
            cpu_seconds = round((after - before) / 1e9, 4)
        samples = memory[label]
        containers[label] = {
            "cpu_seconds": cpu_seconds,
            "mem_mean_mb": round(sum(samples) / len(samples) / 1024**2, 2) if samples else None,
            "mem_peak_mb": round(max(samples) / 1024**2, 2) if samples else None,
            "memory_samples": len(samples),
        }

    out_path.write_text(json.dumps({
        "window_sec": round(window, 2),
        "method": {
            "cpu": "cgroup v1 cpuacct.usage delta, exact CPU time for the window",
            "memory": "docker stats sampled during the window; peak is the largest sample, "
                      "because cgroup v1 memory.max_usage_in_bytes cannot be reset between runs here",
        },
        "containers": containers,
    }, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
