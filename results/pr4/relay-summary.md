# Relay throughput, before and after batched publishing

Workload per trial: 100 requests/s for 30s at full sampling, outbox publish mode.
Before is the original relay, one publish call per outbox row
(`OUTBOX_SINGLE_PUBLISH=true`). After is the same binary publishing each
claimed batch in one call. Median across trials, range in brackets.

| Measurement | Before | After |
| --- | --- | --- |
| Trials | 3 | 3 |
| Requests offered | 3001 (3000–3001) | 3001 (3000–3001) |
| Events mirrored | 3001 (3000–3001) | 3001 (3000–3001) |
| Comparisons stored | 3001 (3000–3001) | 3001 (3000–3001) |
| Peak backlog (events) | 2485 (2469–2485) | 11 (10–11) |
| Queue drain after load (s) | 127 (127–128) | 5 (4–5) |
| Backlog clearing rate (events/s) | 19.4 (19.4–19.6) | 2.2 (2.0–2.8) |
| Confirmed events/s over load + catch-up | 19.1 (19.0–19.1) | 85.7 (85.7–88.2) |
| Confirmation lag p50 (ms) | 61463 (61340–61526) | 83 |
| Confirmation lag p95 (ms) | 116680 (116539–116706) | 104 |
| Confirmation lag max (ms) | 122824 (122680–122839) | 2105 (2094–2105) |
| Relay CPU (s) | 1.41 (1.32–1.42) | 0.58 (0.58–0.62) |
| Relay peak RSS (MB) | 11.1 (8.9–13.5) | 11.1 (10.9–11.5) |
| Client latency p50 (ms) | 0.67 (0.67–0.68) | 0.68 (0.67–0.68) |
| Client latency p95 (ms) | 0.92 (0.92–0.94) | 0.93 (0.93–0.94) |
| Publish failures recorded | 0 | 0 |
| Rows left pending | 0 | 0 |

Every trial stored one comparison per mirrored event, so no events were
lost or duplicated in either configuration (largest difference between
mirrored and stored across all 6 trials: 0).

The backlog clearing rate is the queue depth when the load stopped divided
by how long it took to empty. It describes the relay's own rate only when
there is a backlog to clear: in the after runs the queue never grew past a
dozen events, so that figure measures the poll interval, not throughput.
The relay's own publish rate is measured separately by the diagnostic test.
