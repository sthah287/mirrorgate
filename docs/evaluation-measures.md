# Evaluation measures

What the experiment measures and how each number is produced. This exists
because "does sampling still catch regressions" is not a measurable question
until detection, cost, and overhead are each defined.

## The question

For a fixed workload containing five known faults, how much of the traffic
does MirrorGate have to mirror to still find them, and what does mirroring
cost the user?

## What is held constant

Everything except the sampling strategy:

| | |
|---|---|
| Workload | `experiments/midterm.js`, a fixed request sequence |
| Request rate | 100 per second, constant arrival rate |
| Duration | 30 seconds, so 3000 requests per run |
| Faults | the five in `docs/regression-catalog.md` |
| Services | the same stable and candidate containers |
| Resources | gateway 2 CPUs / 1 GB, stable and candidate 1 CPU / 512 MB each, set in `docker-compose.yml` |
| Starting state | comparisons table truncated, both order tables truncated, recorded external calls cleared, gateway restarted |
| Machine | one laptop, 16 CPUs, Docker Desktop, nothing else running |

The request sequence is driven by the global iteration number rather than by a
random choice per request, so every strategy sees the same requests in the
same order. One request in 100 is the order `POST`.

Each configuration runs 5 times. Trials are the outer loop (all six
strategies, then all six again), so if the machine slows down partway through
it affects every strategy roughly equally instead of ruining one of them.

## The measures

**Total requests.** From k6 (`http_reqs`). The client's view, including
requests that were never mirrored.

**Mirrored requests.** From the gateway's own counter. The database can't
answer this, because a skipped request leaves no row anywhere.

**Actual mirrored rate.** `mirrored / eligible`, as a percentage. This is
measured, not the configured target: a 25% setting lands near 25% but not
exactly on it, and in risk mode there is no single target to compare against.

**Regressions detected.** Of the five faults, how many produced at least one
stored comparison matching the detection rule in the catalog. A fault that is
never mirrored is never detected; that is the point of the experiment.

**Detection rate.** Detected faults over (5 × trials), as a percentage. A
strategy that finds 5 of 5 four times and 4 of 5 once is 96%.

**Time to first detection.** Seconds from the first mirrored request of the
run to the first stored comparison that detects that fault. Both timestamps
are `received_at`, set by the gateway when the request arrived, so they come
from one clock. Using the first mirrored request rather than the moment k6
started keeps everything on that clock; at 100 requests a second the two are
within a few hundred milliseconds.

**Requests until first detection.** Time to first detection × 100, because the
arrival rate is constant. It is an estimate derived from the timing, not a
counted number of rows, and it is the number worth quoting: it says how much
traffic a strategy needed before it noticed.

**Client latency p50, p95, p99.** From k6, measured at the client, through the
gateway. This is the overhead question: the candidate request and the publish
both happen after the client already has its response, so mirroring should
barely show up here.

**Standard deviation.** Reported across the 5 trials for the actual rate and
for p50 and p95, so a difference between strategies can be compared against
the run-to-run noise.

**Side-effect isolation.** Checked on every run, not demonstrated once: live
payment calls must equal the number of orders the client actually sent, and
sandbox payment calls must equal the number of orders that were mirrored. The
result is in each run file as `side_effects.isolation_ok`.

## What is not measured

**CPU and memory.** `docker stats` gives a point-in-time sample, and sampling
it a few times during a 30 second run would not survive being called a
measurement. The resource limits are fixed instead, so no strategy can use
more than another.

**Broker and worker throughput.** The worker drains the topic after k6
finishes and the run waits for it, so the experiment measures what was
detected, not how fast the pipeline processed it.

**Multiple machines.** Everything is one laptop.

## Known weaknesses

- Five faults is a small sample. A detection rate of 80% means four of five,
  not a precise probability.
- Risk mode starts every run cold, with no history. That is realistic for a
  gateway that has just been deployed and it is the same for all five trials,
  but it means these results describe cold-start behaviour.
- The faults are hardcoded in the candidate rather than injected at random, so
  they are in the same place every run.
- 3000 requests is a short run. A regression that only appears under sustained
  load would not show up.
