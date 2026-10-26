# Rare-fault sampling experiment

10 trials per strategy. Same workload every run (`experiments/rare.js`): 3000 or 3001 requests at a constant arrival rate, with each of the five faults appearing once in every 1000 requests, so about three chances per trial to catch each one.

Detection is probabilistic at this fault rate, so each number is a proportion with an interval around it. The interval treats the trial as the unit, by resampling whole trials, because the five faults inside a trial share one gateway and one sampler and are not five independent chances. `off` mirrors nothing and is the cost and latency baseline.

## Detection

| Strategy | Mirrored | Faults found per trial | Detection | 95% CI, trial-clustered |
|---|---|---|---|---|
| off (0%) | 0 / 3001 (0.0%) | 0.0 of 5 | 0% | 0-0% * |
| full (100%) | 3001 / 3001 (100.0%) | 5.0 of 5 | 100% | 100-100% * |
| random 50% | 1513 / 3001 (50.4%) | 4.7 of 5 | 94% | 86-100% |
| random 25% | 748 / 3001 (24.9%) | 2.8 of 5 | 56% | 42-70% |
| random 10% | 295 / 3001 (9.8%) | 1.3 of 5 | 26% | 12-40% |
| endpoint rules | 1250 / 3001 (41.7%) | 4.4 of 5 | 88% | 76-98% |
| risk-priority | 308 / 3001 (10.3%) | 2.3 of 5 | 46% | 38-56% |

\* every trial gave the same answer, so there is no spread to measure and the interval collapses to a point. That is a limit of ten trials rather than evidence of precision.

## Detection probability per fault

Trials where the fault was caught at all, with the 95% interval.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| off (0%) | 0/10 (0%, 0-28) | 0/10 (0%, 0-28) | 0/10 (0%, 0-28) | 0/10 (0%, 0-28) | 0/10 (0%, 0-28) |
| full (100%) | 10/10 (100%, 72-100) | 10/10 (100%, 72-100) | 10/10 (100%, 72-100) | 10/10 (100%, 72-100) | 10/10 (100%, 72-100) |
| random 50% | 9/10 (90%, 60-98) | 10/10 (100%, 72-100) | 9/10 (90%, 60-98) | 10/10 (100%, 72-100) | 9/10 (90%, 60-98) |
| random 25% | 5/10 (50%, 24-76) | 5/10 (50%, 24-76) | 4/10 (40%, 17-69) | 7/10 (70%, 40-89) | 7/10 (70%, 40-89) |
| random 10% | 2/10 (20%, 6-51) | 1/10 (10%, 2-40) | 3/10 (30%, 11-60) | 5/10 (50%, 24-76) | 2/10 (20%, 6-51) |
| endpoint rules | 9/10 (90%, 60-98) | 10/10 (100%, 72-100) | 8/10 (80%, 49-94) | 8/10 (80%, 49-94) | 9/10 (90%, 60-98) |
| risk-priority | 4/10 (40%, 17-69) | 4/10 (40%, 17-69) | 5/10 (50%, 24-76) | 2/10 (20%, 6-51) | 8/10 (80%, 49-94) |

## Exact requests until first detection

The exact position the gateway gave the detecting request, counted from its restart, averaged over the trials where the fault was found. Not an estimate from the request rate.

These runs recorded when the fault-bearing request *arrived*, not when the worker *confirmed* the regression. Those are different: the candidate still has to answer, and R4 deliberately takes the full two second timeout. Confirmation timing is measured separately in `confirmation-timing.json`.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| off (0%) | never found | never found | never found | never found | never found |
| full (100%) | 101 ± 0 | 301 ± 0 | 501 ± 0 | 701 ± 0 | 901 ± 0 |
| random 50% | 657 ± 726 | 601 ± 483 | 945 ± 726 | 1101 ± 699 | 1901 ± 707 |
| random 25% | 901 ± 447 | 1101 ± 837 | 751 ± 500 | 1415 ± 951 | 1758 ± 900 |
| random 10% | 1601 ± 707 | 1301 ± 0 | 1834 ± 1155 | 1501 ± 837 | 2901 ± 0 |
| endpoint rules | 768 ± 707 | 801 ± 850 | 751 ± 707 | 1451 ± 707 | 1679 ± 833 |
| risk-priority | 1601 ± 1000 | 1051 ± 957 | 1101 ± 548 | 1201 ± 707 | 1526 ± 744 |

## Cost

CPU is an exact cgroup counter difference over the run. Memory is the largest sample taken during it, roughly every two seconds.

| Strategy | Gateway CPU | Candidate CPU | Worker CPU | Relay CPU | Total CPU | CPU per 1000 requests | Gateway peak memory |
|---|---|---|---|---|---|---|---|
| off (0%) | 1.35 s | 0.14 s | 0.04 s | 0.09 s | 1.85 s | 0.62 s | 15.6 MB |
| full (100%) | 2.52 s | 0.81 s | 0.37 s | 0.27 s | 5.06 s | 1.69 s | 16.4 MB |
| random 50% | 1.99 s | 0.48 s | 0.37 s | 0.26 s | 3.80 s | 1.27 s | 15.8 MB |
| random 25% | 1.70 s | 0.34 s | 0.35 s | 0.26 s | 3.20 s | 1.07 s | 15.3 MB |
| random 10% | 1.56 s | 0.24 s | 0.19 s | 0.23 s | 2.69 s | 0.90 s | 14.8 MB |
| endpoint rules | 1.99 s | 0.46 s | 0.37 s | 0.29 s | 3.86 s | 1.29 s | 15.9 MB |
| risk-priority | 1.56 s | 0.25 s | 0.20 s | 0.24 s | 2.72 s | 0.90 s | 15.2 MB |

## Client latency through the gateway

| Strategy | p50 | p95 | p99 |
|---|---|---|---|
| off (0%) | 0.51 ms (sd 0.01) | 0.88 ms (sd 0.03) | 1.48 ms |
| full (100%) | 0.59 ms (sd 0.02) | 0.97 ms (sd 0.03) | 3.95 ms |
| random 50% | 0.56 ms (sd 0.02) | 0.96 ms (sd 0.05) | 1.91 ms |
| random 25% | 0.54 ms (sd 0.03) | 0.94 ms (sd 0.03) | 1.44 ms |
| random 10% | 0.52 ms (sd 0.02) | 0.94 ms (sd 0.04) | 1.46 ms |
| endpoint rules | 0.56 ms (sd 0.04) | 0.96 ms (sd 0.09) | 12.37 ms |
| risk-priority | 0.52 ms (sd 0.02) | 0.92 ms (sd 0.04) | 1.42 ms |

## Cost per detection

| Strategy | Mirrored requests per fault caught | CPU seconds per fault caught |
|---|---|---|
| off (0%) | - | - |
| full (100%) | 600 | 1.0 |
| random 50% | 322 | 0.8 |
| random 25% | 267 | 1.1 |
| random 10% | 227 | 2.1 |
| endpoint rules | 284 | 0.9 |
| risk-priority | 134 | 1.2 |

Shadow write isolation held in 70 of 70 runs.

