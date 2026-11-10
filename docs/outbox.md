# The outbox

Until week 8 the gateway published comparison events straight to Redpanda. If
the broker was unavailable the event was counted in `publish_failed` and
dropped. The client never noticed, but the comparison was gone.

That matters more than it sounds. Once the experiment's faults are rare, each
one only appears about three times in a trial, so a single lost event can turn
a detected fault into a missed one. Durability is not a nicety here; without
it there is no way to tell "sampling missed it" from "the broker ate it".

## How it works now

```text
gateway ──► comparison_outbox (Postgres) ──► outbox-relay ──► Redpanda ──► worker
```

The gateway writes the event as a row and moves on. A separate service, the
relay, polls for unpublished rows, publishes them, and marks them done. A
broker outage means rows pile up in Postgres and go out when it comes back.

The table:

| column | |
|---|---|
| `request_id` | primary key, so writing the same event twice is a no-op |
| `payload` | the serialized event |
| `traceparent` | the W3C trace context, see below |
| `created_at` | when the gateway wrote it |
| `published_at` | null until the relay has published it |
| `attempts`, `last_error` | why it is still sitting there |

The relay claims a batch with `SELECT ... FOR UPDATE SKIP LOCKED`, publishes
the whole batch in one call, marks the rows the broker acknowledged, and
commits. The lock is held for the whole batch, which is why there is a publish
timeout on it. `SKIP LOCKED` means a second relay would pick up different rows
rather than block, so running two is safe even though one is enough here.

## Throughput, and why it was 20 events a second

Week 9 measured the pipeline keeping up with about 20 events a second against
100 requests a second arriving, so a backlog built for the whole run and took
about two minutes to clear. The suspected cause was the relay publishing one
message per call through a writer with a 50 ms batch timeout. That was
arithmetic, not a measurement, so week 10 measured the three candidate costs
separately against the real Redpanda and Postgres
(`outbox-relay/internal/relay/diagnostic_test.go`):

| | per event | rate |
|---|---|---|
| one publish call per row | 50.6 ms | 20/s |
| the same 100 rows in one call | under 0.01 ms | 124,760/s |
| claiming and marking 100 rows in Postgres | 0.04 ms | not the bottleneck |

The database side is 0.08% of the cost. The 50.6 ms matches the writer's 50 ms
`BatchTimeout` almost exactly: every call waited out the timer because there
was never a second message to batch with. The batched rate moves between runs
because a sub-millisecond measurement of 100 messages has little resolution;
the single-publish figure is the stable one, and it is the one that mattered.

Publishing the claimed rows in one call removed it. Same workload, three
trials each:

| | one publish per row | batched |
|---|---|---|
| peak backlog | 2,485 events | 11 events |
| queue drain after the load stopped | 127 s | 5 s |
| confirmation delay, median | 61.5 s | 83 ms |
| confirmation delay, p95 | 116.7 s | 104 ms |
| relay CPU per run | 1.41 s | 0.58 s |

The slowest confirmation after the change is about 2.1 s, which is R4's
deliberate two second candidate timeout rather than pipeline delay.

`OUTBOX_SINGLE_PUBLISH=true` puts the relay back to one call per row, so the
before column can be reproduced without checking out an old commit.

### A batch can half succeed

Publishing one message at a time made failure simple: it worked or it did not.
A batch can be partly accepted, and marking a row published that the broker
never acknowledged would lose the event silently, which is exactly what the
outbox exists to prevent. So the publisher returns the request ids the broker
acknowledged alongside the error:

- **all acknowledged**: every row is marked, as before.
- **some acknowledged**: kafka-go reports this as `kafka.WriteErrors`, one
  entry per message. Only the acknowledged ids are marked; the rest stay
  pending and go out on the next pass.
- **outcome unknown**, such as a connection reset with no per-message detail:
  nothing is marked. The whole batch is retried, and the worker drops the
  duplicates.

The third case is the one that matters. It is better to send an event twice
than to assume an unacknowledged event arrived, and at-least-once delivery
plus the idempotent insert makes the duplicate harmless. There are tests for
all three.

## What is actually guaranteed

**Once the outbox row is committed, the event will reach the topic.** The
relay keeps retrying until it succeeds, and a crash just means the rows are
still there when it comes back.

The guarantee attaches to the committed row, not to the request, and that
distinction is the whole boundary. An event that was persisted is safe. An
event that was never persisted, because the gateway died between finishing the
shadow request and committing the insert, is gone and nothing downstream knows
it existed. Across 70 experiment runs the gateway's mirrored counter and the
stored comparison count matched in every run, so in practice nothing fell into
that window, but it is a window.

That is the whole promise, and these are the edges of it:

- **It is not atomic with the stable service's write.** The stable service has
  its own database and its own transaction. MirrorGate's outbox commits in a
  different database in a different service, so the two can disagree if one
  side fails. The outbox is about not losing MirrorGate's own comparison
  record, nothing more.
- **A gateway crash between the shadow request finishing and the row
  committing loses that event.** There is no journal in front of the outbox.
  The window is one insert wide, but it is not zero.
- **Delivery is at least once, not exactly once.** If the relay publishes a
  batch and then fails before committing, those events are published again on
  the next pass. The worker handles this: `SaveComparison` is
  `ON CONFLICT (request_id) DO NOTHING`, so a duplicate is dropped instead of
  creating a second comparison. There is a test for it.
- **Ordering is per request, not global.** Events are keyed by request id, and
  nothing depends on the order they arrive in.

## The trace had to travel in the row

The trace context used to be injected at publish time from the live request
context. With the relay publishing seconds or minutes later, that context no
longer exists, and the worker's spans would have started a brand new trace
with no link back to the request.

So the gateway stores the traceparent in the row, and the relay rebuilds the
context from it before publishing. The relay also opens its own span, so the
hop shows up rather than being an unexplained gap. One mirrored request is
still one trace, now with five services in it:

```text
gateway            GET
gateway              GET stable-service:9001
gateway              GET candidate-service:9002
stable-service       stable-service
candidate-service    candidate-service
gateway              publish comparison event
outbox-relay         relay publish          <- through Postgres and Kafka
comparison-worker    process comparison
comparison-worker      save comparison
```

A test asserts the stored traceparent is the one used at publish time, because
this is the kind of thing that breaks silently: the spans still appear, they
are just in the wrong trace.

## Settings

Gateway:

| | | |
|---|---|---|
| `PUBLISH_MODE` | `outbox` | `direct` restores the old straight-to-Kafka path |

`direct` exists so the experiment can measure what the outbox is worth. It is
not a mode to run for real.

Relay:

| | | |
|---|---|---|
| `OUTBOX_BATCH_SIZE` | `100` | rows claimed per pass |
| `OUTBOX_POLL_INTERVAL` | `200ms` | how often it looks when idle |
| `OUTBOX_MAX_BACKOFF` | `10s` | longest wait while the broker is down |
| `OUTBOX_PUBLISH_TIMEOUT` | `10s` | bounds one batch, and the lock it holds |
| `OUTBOX_RETENTION` | `1h` | published rows are deleted after this |
| `OUTBOX_SINGLE_PUBLISH` | `false` | `true` publishes one row per call, the pre-week-10 behaviour |

Retention matters: without it the table grows for the length of an experiment
and never shrinks.

## Checking it

```bash
./scripts/failure-test.sh
```

Five scenarios: the worker down, the broker down with the outbox, the relay
killed mid-run, the broker down in `direct` mode for comparison, and the same
events delivered twice. The last
one is the measurement that makes the rest worth anything, and it is worth
running more than once: across four runs it lost 10, 1, 0 and 10 of 10 events.
The outbox scenarios recovered 10 of 10 every time.

The redelivery scenario clears `published_at` on rows the relay has already
sent, which is what a crash between publishing and committing looks like from
the database's side, and checks that the second delivery produces no second
comparison row.

`scripts/trace-check.sh` is the other half of the batching change: it sends a
burst, so several events are published together, and asks Jaeger whether each
request still has one trace covering the gateway, the relay and the worker.
Twenty of twenty, with twenty distinct trace ids, in the run recorded for
week 10.

`scripts/direct-mode-probe.sh` explains the variability. It restores the
broker and sends no further traffic, and the events still arrive, so the
Kafka client is retrying in the background after the gateway's publish call
has already timed out and counted a failure. What decides the outcome is
whether the broker returns inside the client's retry budget. That is why
`publish_failed` counts deadline expiries rather than losses, and why the
number recovered is unpredictable from one run to the next.

The current depth is on the internal API and the dashboard:

```bash
curl -s http://localhost:8081/internal/stats | python -m json.tool
```

```json
"outbox": {"pending": 0, "published": 318, "oldest_pending_sec": 0, "failed_attempts": 0}
```

`pending` climbing with `oldest_pending_sec` growing is what a broker outage
looks like from the outside.
