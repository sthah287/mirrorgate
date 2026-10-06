# Midterm sampling experiment

5 trials per strategy, same workload (`experiments/midterm.js`) every time: 3000 requests at a constant arrival rate, five known regressions (R1-R5 in `docs/regression-catalog.md`). Means across trials.

## Cost and detection

| Strategy | Mirrored | Actual rate | Stored | Regressions found | Detection rate |
|---|---|---|---|---|---|
| full (100%) | 3000 / 3000 | 100.0% (sd 0.0) | 3000 | 5.0 of 5 | 100% |
| random 50% | 1510 / 3000 | 50.3% (sd 0.9) | 1510 | 5.0 of 5 | 100% |
| random 25% | 745 / 3001 | 24.8% (sd 0.4) | 745 | 5.0 of 5 | 100% |
| random 10% | 313 / 3001 | 10.4% (sd 0.3) | 313 | 5.0 of 5 | 100% |
| endpoint rules | 1268 / 3000 | 42.3% (sd 1.0) | 1268 | 5.0 of 5 | 100% |
| risk-priority | 406 / 3000 | 13.5% (sd 0.6) | 406 | 5.0 of 5 | 100% |

## Client latency through the gateway

| Strategy | p50 | p95 | p99 |
|---|---|---|---|
| full (100%) | 0.61 ms (sd 0.01) | 1.03 ms (sd 0.03) | 2.99 ms |
| random 50% | 0.60 ms (sd 0.07) | 1.00 ms (sd 0.05) | 2.73 ms |
| random 25% | 0.58 ms (sd 0.08) | 0.99 ms (sd 0.02) | 2.63 ms |
| random 10% | 0.53 ms (sd 0.02) | 0.96 ms (sd 0.02) | 2.67 ms |
| endpoint rules | 0.59 ms (sd 0.02) | 1.03 ms (sd 0.02) | 2.77 ms |
| risk-priority | 0.58 ms (sd 0.09) | 0.99 ms (sd 0.05) | 2.99 ms |

Shadow write isolation held in 30 of 30 runs: one live payment per order the client sent, and sandbox payments only for mirrored orders.

## Per regression

Trials where it was found, and the mean request count to the first detection in those trials, plus or minus the standard deviation across them. The spread matters as much as the mean: where two strategies overlap inside it, they are not distinguishable here.

| Strategy | R1 | R2 | R3 | R4 | R5 |
|---|---|---|---|---|---|
| full (100%) | 5/5, ~11 ± 0 req | 5/5, ~13 ± 0 req | 5/5, ~14 ± 0 req | 5/5, ~15 ± 0 req | 5/5, ~0 ± 0 req |
| random 50% | 5/5, ~18 ± 11 req | 5/5, ~20 ± 11 req | 5/5, ~33 ± 36 req | 5/5, ~22 ± 10 req | 5/5, ~139 ± 113 req |
| random 25% | 5/5, ~20 ± 12 req | 5/5, ~34 ± 17 req | 5/5, ~35 ± 24 req | 5/5, ~72 ± 37 req | 5/5, ~357 ± 320 req |
| random 10% | 5/5, ~181 ± 221 req | 5/5, ~171 ± 84 req | 5/5, ~95 ± 71 req | 5/5, ~305 ± 235 req | 5/5, ~1190 ± 469 req |
| endpoint rules | 5/5, ~11 ± 1 req | 5/5, ~24 ± 26 req | 5/5, ~33 ± 20 req | 5/5, ~86 ± 51 req | 5/5, ~159 ± 229 req |
| risk-priority | 5/5, ~83 ± 61 req | 5/5, ~88 ± 61 req | 5/5, ~218 ± 148 req | 5/5, ~94 ± 74 req | 5/5, ~355 ± 133 req |

