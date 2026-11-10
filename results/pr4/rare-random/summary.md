# Rare-fault sampling experiment

20 trials per strategy. Same workload every run (`experiments/rare.js`): 3000 or 3001 requests at a constant arrival rate, with each of the five faults appearing once in every 1000 requests, so about three chances per trial to catch each one.

Detection is probabilistic at this fault rate, so each number is a proportion with an interval around it. The interval treats the trial as the unit, by resampling whole trials, because the five faults inside a trial share one gateway and one sampler and are not five independent chances. `off` mirrors nothing and is the cost and latency baseline.

## Detection

| Strategy | Mirrored | Faults found per trial | Detection | 95% CI, trial-clustered |
|---|---|---|---|---|
| off (0%) | 0 / 3001 (0.0%) | 0.0 of 5 | 0% | 0-0% * |
| full (100%) | 3000 / 3000 (100.0%) | 5.0 of 5 | 100% | 100-100% * |
| random 50% | 1494 / 3001 (49.8%) | 4.3 of 5 | 86% | 80-91% |
| random 25% | 747 / 3000 (24.9%) | 3.2 of 5 | 64% | 56-71% |
| random 10% | 305 / 3001 (10.2%) | 1.4 of 5 | 28% | 20-36% |
| endpoint rules | 1239 / 2997 (41.3%) | 4.2 of 5 | 85% | 77-92% |
| risk-priority | 627 / 3001 (20.9%) | 2.6 of 5 | 53% | 44-62% |
| risk-urls | 305 / 3001 (10.2%) | 3.0 of 5 | 60% | 51-69% |

\* every trial gave the same answer, so there is no spread to measure and the interval collapses to a point. That is a limit of 20 trials rather than evidence of precision.

## Detection probability per fault

Trials where the fault was caught at all, with the 95% interval.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| off (0%) | 0/20 (0%, 0-16) | 0/20 (0%, 0-16) | 0/20 (0%, 0-16) | 0/20 (0%, 0-16) | 0/20 (0%, 0-16) |
| full (100%) | 20/20 (100%, 84-100) | 20/20 (100%, 84-100) | 20/20 (100%, 84-100) | 20/20 (100%, 84-100) | 20/20 (100%, 84-100) |
| random 50% | 17/20 (85%, 64-95) | 18/20 (90%, 70-97) | 19/20 (95%, 76-99) | 17/20 (85%, 64-95) | 15/20 (75%, 53-89) |
| random 25% | 13/20 (65%, 43-82) | 12/20 (60%, 39-78) | 13/20 (65%, 43-82) | 12/20 (60%, 39-78) | 14/20 (70%, 48-85) |
| random 10% | 3/20 (15%, 5-36) | 7/20 (35%, 18-57) | 5/20 (25%, 11-47) | 6/20 (30%, 15-52) | 7/20 (35%, 18-57) |
| endpoint rules | 17/20 (85%, 64-95) | 20/20 (100%, 84-100) | 17/20 (85%, 64-95) | 14/20 (70%, 48-85) | 17/20 (85%, 64-95) |
| risk-priority | 7/20 (35%, 18-57) | 7/20 (35%, 18-57) | 14/20 (70%, 48-85) | 7/20 (35%, 18-57) | 18/20 (90%, 70-97) |
| risk-urls | 11/20 (55%, 34-74) | 13/20 (65%, 43-82) | 13/20 (65%, 43-82) | 9/20 (45%, 26-66) | 14/20 (70%, 48-85) |

## Requests between a fault first appearing and being caught

The fault positions are drawn from a seed, so a detection's absolute position says more about where the fault landed than about the strategy. This is the gap instead: how much traffic passed between the first occurrence of the fault and the request that caught it, averaged over the trials where it was caught. Zero means the first occurrence was the one caught.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| off (0%) | never found | never found | never found | never found | never found |
| full (100%) | 0 ± 0 | 0 ± 0 | 0 ± 0 | 0 ± 0 | 0 ± 0 |
| random 50% | 458 ± 706 | 644 ± 800 | 598 ± 639 | 446 ± 766 | 153 ± 279 |
| random 25% | 632 ± 806 | 558 ± 833 | 712 ± 554 | 492 ± 482 | 506 ± 714 |
| random 10% | 450 ± 779 | 315 ± 401 | 687 ± 841 | 186 ± 276 | 1217 ± 1156 |
| endpoint rules | 652 ± 802 | 410 ± 715 | 305 ± 459 | 604 ± 743 | 327 ± 653 |
| risk-priority | 271 ± 626 | 334 ± 661 | 772 ± 735 | 0 ± 0 | 419 ± 678 |
| risk-urls | 1195 ± 856 | 595 ± 632 | 175 ± 524 | 962 ± 1082 | 720 ± 939 |

## How long the pipeline took to confirm a fault it had seen

Milliseconds from the fault-bearing request arriving at the gateway to the worker storing the comparison that caught it, averaged over the trials where it was caught. Not the time from the start of the run: with the positions randomised, that number mostly says when the fault happened to occur. This one is the part the system controls, and it includes the candidate still having to answer, which is why R4 is about two seconds everywhere: it spends them waiting out the shadow timeout on purpose.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| off (0%) | never found | never found | never found | never found | never found |
| full (100%) | 87 ms | 86 ms | 487 ms | 2,102 ms | 100 ms |
| random 50% | 103 ms | 85 ms | 491 ms | 2,089 ms | 90 ms |
| random 25% | 131 ms | 142 ms | 508 ms | 2,129 ms | 176 ms |
| random 10% | 206 ms | 131 ms | 534 ms | 2,159 ms | 185 ms |
| endpoint rules | 109 ms | 110 ms | 493 ms | 2,117 ms | 108 ms |
| risk-priority | 132 ms | 93 ms | 524 ms | 2,388 ms | 137 ms |
| risk-urls | 181 ms | 156 ms | 552 ms | 2,158 ms | 166 ms |

## Cost

CPU is an exact cgroup counter difference over the run. Memory is the largest sample taken during it, roughly every two seconds.

| Strategy | Gateway CPU | Candidate CPU | Worker CPU | Relay CPU | Total CPU | CPU per 1000 requests | Gateway peak memory |
|---|---|---|---|---|---|---|---|
| off (0%) | 1.40 s | 0.15 s | 0.07 s | 0.09 s | 1.92 s | 0.64 s | 16.1 MB |
| full (100%) | 2.74 s | 0.80 s | 1.11 s | 0.66 s | 7.20 s | 2.40 s | 18.8 MB |
| random 50% | 2.15 s | 0.53 s | 0.64 s | 0.51 s | 5.18 s | 1.73 s | 17.4 MB |
| random 25% | 1.86 s | 0.34 s | 0.37 s | 0.35 s | 3.68 s | 1.23 s | 17.1 MB |
| random 10% | 1.66 s | 0.24 s | 0.21 s | 0.23 s | 2.82 s | 0.94 s | 16.2 MB |
| endpoint rules | 2.00 s | 0.44 s | 0.54 s | 0.49 s | 4.47 s | 1.49 s | 16.9 MB |
| risk-priority | 1.69 s | 0.31 s | 0.31 s | 0.28 s | 3.23 s | 1.08 s | 17.1 MB |
| risk-urls | 1.62 s | 0.24 s | 0.20 s | 0.22 s | 2.75 s | 0.92 s | 16.4 MB |

## Client latency through the gateway

| Strategy | p50 | p95 | p99 |
|---|---|---|---|
| off (0%) | 0.51 ms (sd 0.03) | 0.89 ms (sd 0.07) | 1.47 ms |
| full (100%) | 0.63 ms (sd 0.06) | 0.98 ms (sd 0.08) | 16.96 ms |
| random 50% | 0.59 ms (sd 0.06) | 0.98 ms (sd 0.10) | 7.68 ms |
| random 25% | 0.57 ms (sd 0.07) | 0.96 ms (sd 0.10) | 22.31 ms |
| random 10% | 0.55 ms (sd 0.06) | 0.95 ms (sd 0.10) | 4.13 ms |
| endpoint rules | 0.58 ms (sd 0.07) | 4.29 ms (sd 14.92) | 49.77 ms |
| risk-priority | 0.54 ms (sd 0.05) | 0.93 ms (sd 0.08) | 1.58 ms |
| risk-urls | 0.53 ms (sd 0.08) | 0.94 ms (sd 0.11) | 3.99 ms |

## Cost per detection

| Strategy | Mirrored requests per fault caught | CPU seconds per fault caught |
|---|---|---|
| off (0%) | - | - |
| full (100%) | 600 | 1.4 |
| random 50% | 347 | 1.2 |
| random 25% | 234 | 1.1 |
| random 10% | 218 | 2.0 |
| endpoint rules | 291 | 1.1 |
| risk-priority | 236 | 1.2 |
| risk-urls | 102 | 0.9 |

Shadow write isolation held in 160 of 160 runs.

