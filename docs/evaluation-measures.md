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

**Time to first detection.** Milliseconds from the first mirrored request of
the run to the detecting one, both `received_at` on the gateway clock.

The anchor is the first mirrored request rather than the gateway process
start, because the harness restarts the gateway and then waits for health
checks and for k6 to start, and those seconds are not part of detecting
anything. The run files also carry the raw figure measured from process
start, which is the same number plus that startup gap.

**Requests until first detection.** The exact position of the detecting
request, not an estimate.

This changed in week 9. The midterm multiplied elapsed time by the request
rate, which is an estimate and was marked down as one. The gateway now gives
every eligible request a sequence number, counted from 1 when the process
started, and that number is stored with the comparison. The harness restarts
the gateway before each run, so the sequence number is the request's position
in that run. "Caught at request 901" is now a counted integer with no clock
and no arithmetic behind it.

The three times involved are deliberately kept apart, because they come from
different places:

| | source | used for |
|---|---|---|
| `request_seq` | gateway counter | requests until detection. No clock at all |
| `received_at` | gateway clock | when the request arrived |
| `detected_at` | worker clock | when the comparison was stored |

`received_at` and the gateway's `started_at` are the same clock, so they can
be subtracted. `detected_at` belongs to a different process, so it is only
used for pipeline lag and is never mixed into a detection time.

**Client latency p50, p95, p99.** From k6, measured at the client, through the
gateway. This is the overhead question: the candidate request and the publish
both happen after the client already has its response, so mirroring should
barely show up here.

**Detection probability.** Once the faults are rare, a strategy either catches
a fault in a trial or it does not, so the measure is a proportion of trials
with a 95% confidence interval around it.

The interval is Wilson, not the normal approximation. With ten trials the
normal interval misbehaves near 0 and 1, including running past the ends of
the scale, which is exactly where several of these results sit.

**Cost per detection.** Mirrored requests per fault caught, and CPU seconds
per fault caught. Detection on its own says nothing about whether a strategy
is worth using.

**Standard deviation.** Reported across trials for the actual rate and for
p50 and p95, so a difference between strategies can be compared against the
run-to-run noise.

**Side-effect isolation.** Checked on every run, not demonstrated once: live
payment calls must equal the number of orders the client actually sent, and
sandbox payment calls must equal the number of orders that were mirrored. The
result is in each run file as `side_effects.isolation_ok`.

**CPU.** Exact. cgroup v1 exposes `cpuacct.usage`, a cumulative nanosecond
counter per container, so the difference between the start and the end of a
run is precisely the CPU time that container used during it. Nothing is
sampled. Reported per container, as a total, and per 1000 requests.

The midterm skipped this and said sampling `docker stats` would not survive
being called a measurement. That was true of sampling, and it was also the
wrong conclusion: the counter difference is exact and was there all along.

**Memory.** Sampled, and weaker than the CPU numbers.

This machine runs cgroup v1, where the peak counter is
`memory.max_usage_in_bytes` and it is cumulative since the container started.
The cgroup mount inside the container is read only, so it cannot be reset
between runs, and using it would report the largest value since boot rather
than the largest during this run. Instead `memory.usage_in_bytes` is sampled
through `docker stats` for the length of the run, and the peak is the largest
sample. One call covering every container takes about two seconds, so that is
roughly the sampling interval, and a spike shorter than the gap between two
samples can be missed.

## What is not measured

**Broker, relay and worker throughput.** The outbox and the topic both drain
after k6 finishes and the run waits for both, so the experiment measures what
was detected, not how fast the pipeline processed it. Outbox depth during the
run is recorded but not analysed.

**Multiple machines.** Everything is one laptop.

## The rare-fault workload

The midterm used faults common enough that every strategy found all five every
time, which made detection rate useless as a measure and left the research
question unanswered. `experiments/rare.js` puts each fault at a fixed position
once in every 1000 requests, so a 3000 request trial gives about three chances
to catch each one.

With P(detect) = 1 - (1 - rate)^3 that is roughly 27% at 10% sampling, 58% at
25%, 88% at 50% and 100% at full, which is enough spread for the strategies to
separate. Rarity comes from the workload rather than from changes to the
candidate service, so the regression catalog still describes the same faults.

## Known weaknesses

- Five faults is a small sample. A detection rate of 80% means four of five,
  not a precise probability.
- Ten trials gives wide confidence intervals. They are reported as they come
  out rather than narrowed by wishful rounding.
- Memory is sampled at roughly two second intervals, so short spikes are
  invisible. The CPU numbers do not have this problem.
- Risk mode starts every run cold, with no history. That is realistic for a
  gateway that has just been deployed and it is the same for all five trials,
  but it means these results describe cold-start behaviour.
- The faults are hardcoded in the candidate rather than injected at random, so
  they are in the same place every run.
- 3000 requests is a short run. A regression that only appears under sustained
  load would not show up.
