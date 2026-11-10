# Rare-fault sampling experiment

3 trials per strategy. Same workload every run (`experiments/rare.js`): 3000 or 3001 requests at a constant arrival rate, with each of the five faults appearing once in every 1000 requests, so about three chances per trial to catch each one.

Detection is probabilistic at this fault rate, so each number is a proportion with an interval around it. The interval treats the trial as the unit, by resampling whole trials, because the five faults inside a trial share one gateway and one sampler and are not five independent chances. `off` mirrors nothing and is the cost and latency baseline.

## Detection

| Strategy | Mirrored | Faults found per trial | Detection | 95% CI, trial-clustered |
|---|---|---|---|---|
| risk-priority | 310 / 3001 (10.3%) | 2.3 of 5 | 47% | 40-60% |
| risk-urls | 325 / 3001 (10.8%) | 2.7 of 5 | 53% | 20-80% |

\* every trial gave the same answer, so there is no spread to measure and the interval collapses to a point. That is a limit of ten trials rather than evidence of precision.

## Detection probability per fault

Trials where the fault was caught at all, with the 95% interval.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| risk-priority | 0/3 (0%, 0-56) | 1/3 (33%, 6-79) | 3/3 (100%, 44-100) | 0/3 (0%, 0-56) | 3/3 (100%, 44-100) |
| risk-urls | 2/3 (67%, 21-94) | 1/3 (33%, 6-79) | 0/3 (0%, 0-56) | 2/3 (67%, 21-94) | 3/3 (100%, 44-100) |

## Exact requests until first detection

The exact position the gateway gave the detecting request, counted from its restart, averaged over the trials where the fault was found. Not an estimate from the request rate.

These runs recorded when the fault-bearing request *arrived*, not when the worker *confirmed* the regression. Those are different: the candidate still has to answer, and R4 deliberately takes the full two second timeout. Confirmation timing is measured separately in `confirmation-timing.json`.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| risk-priority | never found | 2301 ± 0 | 501 ± 0 | never found | 1234 ± 577 |
| risk-urls | 1101 ± 0 | 1301 ± 0 | never found | 1701 ± 0 | 1568 ± 1155 |

## Cost

CPU is an exact cgroup counter difference over the run. Memory is the largest sample taken during it, roughly every two seconds.

| Strategy | Gateway CPU | Candidate CPU | Worker CPU | Relay CPU | Total CPU | CPU per 1000 requests | Gateway peak memory |
|---|---|---|---|---|---|---|---|
| risk-priority | 1.60 s | 0.22 s | 0.17 s | 0.21 s | 2.70 s | 0.90 s | 17.6 MB |
| risk-urls | 1.64 s | 0.25 s | 0.18 s | 0.22 s | 2.81 s | 0.94 s | 17.3 MB |

## Client latency through the gateway

| Strategy | p50 | p95 | p99 |
|---|---|---|---|
| risk-priority | 0.54 ms (sd 0.02) | 0.90 ms (sd 0.02) | 1.33 ms |
| risk-urls | 0.54 ms (sd 0.02) | 0.89 ms (sd 0.02) | 1.34 ms |

## Cost per detection

| Strategy | Mirrored requests per fault caught | CPU seconds per fault caught |
|---|---|---|
| risk-priority | 133 | 1.2 |
| risk-urls | 122 | 1.1 |

Shadow write isolation held in 6 of 6 runs.

