# Running more than one comparison worker

The comparison workers are one Kafka consumer group, so the topic's
partitions decide how many of them can do anything at all. A consumer group
hands each partition to exactly one member: on a one-partition topic a second
worker joins the group, is assigned nothing, and sits idle.

Until week 12 the topic had one partition, so the pipeline could only ever be
one worker wide.

## Partitions

`KAFKA_PARTITIONS` (default 3) is what the gateway asks for at startup. A
topic that already exists with fewer is grown rather than left alone:

```text
PARTITION  LEADER  EPOCH  REPLICAS  LOG-START-OFFSET  HIGH-WATERMARK
0          0       25     [0]       109749            149113
1          0       1      [0]       0                 0
2          0       1      [0]       0                 0
```

Partition 0 keeps its history. Growing a topic is additive, so nothing is
deleted and no volume is reset; the new partitions simply start empty.

Growing changes which partition a key hashes to from then on, which would
matter if ordering per key mattered. Here each request id produces exactly one
event and the worker's insert is idempotent, so it does not.

## What was measured

The same workload, 3000 requests at 100/s with everything mirrored, run at
each size. Per-worker counts are taken from the workers' own logs: each one
logs every event it processes with the request id, so this is a count rather
than an inference from offsets.

| | partitions held | events handled | stored | duplicate rows |
|---|---|---|---|---|
| one worker | 3 | 3001 | 3001 of 3001 | 0 |
| two workers | 2 and 1 | 1987 and 1014 | 3001 of 3001 | 0 |
| second worker added mid-run | 2 and 1 | 1893 and 1108 | 3001 of 3001 | 0 |

The split follows the partition assignment, not the load. With three
partitions and two members, one member gets two partitions and the other gets
one, so the work divides about 2:1 rather than evenly. Six partitions would
divide 1:1 across two workers and 2:1 across three. Partition count is the
thing to choose, and the sensible choice is a number with several divisors.

CPU divides the same way: 0.81 s and 0.47 s for the two workers against
1.21 s for the one, so the total work is the same and it moved rather than
multiplied.

## Rebalance

The third row is the interesting one: the second worker joins ten seconds
into a run that is already in flight. The group rebalances, partitions are
revoked from the first worker and handed to the second, and the first worker
may have a message in hand whose partition it no longer owns.

Nothing was lost and nothing was stored twice: 3001 mirrored requests, 3001
comparisons stored, 3001 distinct request ids.

No request id was delivered to both workers in this run, which is a thing
that happened rather than a thing that is guaranteed. A rebalance between a
message being handled and its offset being committed would deliver that
message again to its new owner, and the design relies on the duplicate being
absorbed: the worker commits only after a successful save, and the save is
`ON CONFLICT (request_id) DO NOTHING`. That is the same at-least-once
property the outbox already depends on, which is why redelivery is tested
directly rather than waited for
(`scripts/failure-test.sh`, scenario 5).

## Running it

```bash
docker compose up -d --scale comparison-worker=2 comparison-worker
./experiments/run-scaling.sh            # one worker, then two, then a rebalance
```

The harness waits for the consumer group's lag to reach zero and checks the
gateway's counters are at zero before each run, so events left in the topic
by an earlier run cannot be counted as this one's work. Without that check an
interrupted run leaked about 500 events into the next measurement, which is
how the check came to exist.

## What this does not establish

Two workers were never the bottleneck at this rate. One worker keeps up with
100 requests a second on its own, so these runs show the work being shared
and nothing being lost, not a throughput gain. Showing a gain needs a load
that one worker cannot keep up with, which on this machine means a rate high
enough that the gateway and Postgres become the limit instead.

The relay is still single. Its `SELECT ... FOR UPDATE SKIP LOCKED` claim
means a second instance would take different rows rather than block, so
running two should work, and it has not been tested.
