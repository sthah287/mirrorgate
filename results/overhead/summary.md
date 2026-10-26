# Mirroring overhead

5 trials with mirroring off and 5 with everything mirrored, at 500 requests per second, about 9983 requests per run. Same workload, same container limits, same reset between runs.

## Client latency

| | off | full | difference |
|---|---|---|---|
| p50 | 0.434 ms (sd 0.014) | 0.484 ms (sd 0.007) | +0.050 ms (95% CI +0.034 to +0.066, measurable) |
| p95 | 0.689 ms (sd 0.077) | 4.141 ms (sd 7.424) | +3.452 ms (95% CI -4.205 to +11.109, no measurable difference) |
| p99 | 1.129 ms (sd 0.073) | 29.784 ms (sd 61.514) | +28.655 ms (95% CI -34.782 to +92.092, no measurable difference) |

## CPU, exact cgroup counter differences

| Container | off | full | difference |
|---|---|---|---|
| gateway | 3.84 s | 8.59 s | +4.752 s (95% CI +3.771 to +5.733, measurable) |
| candidate-service | 0.12 s | 2.52 s | +2.399 s (95% CI +1.704 to +3.094, measurable) |
| comparison-worker | 0.03 s | 0.29 s | +0.257 s (95% CI +0.249 to +0.264, measurable) |
| outbox-relay | 0.07 s | 0.22 s | +0.146 s (95% CI +0.138 to +0.153, measurable) |
| postgres | 0.19 s | 2.99 s | +2.800 s (95% CI +1.359 to +4.240, measurable) |

Total measured CPU: 4.26 s with mirroring off, 14.62 s with everything mirrored, so mirroring cost +10.35 CPU seconds (+243%) for 9983 requests.

## Peak memory, largest sample during the run

| Container | off | full |
|---|---|---|
| gateway | 17.3 MB | 22.4 MB |
| candidate-service | 20.2 MB | 22.8 MB |
| comparison-worker | 11.3 MB | 11.3 MB |
| outbox-relay | 12.1 MB | 12.2 MB |
| postgres | 54.6 MB | 82.3 MB |

Note: 165 shadow requests were skipped because the in-flight limit was full, so at this rate the gateway was already shedding some mirroring work.

