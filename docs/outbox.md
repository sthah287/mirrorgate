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
each row, marks them, and commits. The lock is held for the whole batch, which
is why there is a publish timeout on it. `SKIP LOCKED` means a second relay
would pick up different rows rather than block, so running two is safe even
though one is enough here.

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

Retention matters: without it the table grows for the length of an experiment
and never shrinks.

## Checking it

```bash
./scripts/failure-test.sh
```

Four scenarios: the worker down, the broker down with the outbox, the relay
killed mid-run, and the broker down in `direct` mode for comparison. The last
one is the measurement that makes the rest worth anything, and it is worth
running more than once: across four runs it lost 10, 1, 0 and 10 of 10 events.
The outbox scenarios recovered 10 of 10 every time.

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
