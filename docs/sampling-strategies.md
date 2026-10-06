# Sampling strategies

Mirroring every request doubles the work the backend does. Sampling is the
part of this project that asks how little traffic is enough to still catch a
regression. The decision is made in `gateway/internal/sampling` before any
shadow work starts, so a skipped request costs one rate lookup: no request
copy, no goroutine, no candidate call.

Set with `SAMPLING_MODE` on the gateway.

| Mode | Rate comes from |
|---|---|
| `full` | always 100% |
| `random` | fixed `SAMPLING_RATE` for everything |
| `endpoint` | per-path rules in `SAMPLING_RULES`, `SAMPLING_RATE` as the fallback |
| `risk` | a score per endpoint, recalculated as results come in |

`full`, `random`, and `endpoint` are from week 5. `risk` was added in week 7,
because the proposal said MirrorGate would prioritise interesting requests
without ever saying what that meant.

## Risk-priority

Each endpoint, keyed as `METHOD /path`, gets a score. Every rule is a flat
number that either applies or doesn't, so any rate can be explained in one
line.

| Points | Rule | Why |
|---|---|---|
| +3 | a `different` comparison in the window | the endpoint has already shown a regression |
| +3 | a candidate error or timeout in the window | same, for the worse kind of failure |
| +2 | rare endpoint | random sampling rarely lands on it, so it needs help |
| +2 | write request (`POST`/`PUT`/`PATCH`/`DELETE`) | a wrong write costs more than a wrong read |
| +1 | a latency regression in the window | worth watching, but not as urgent as a wrong answer |

Score to sampling rate:

| Score | Rate |
|---|---|
| 0–1 | 10% |
| 2–3 | 25% |
| 4–5 | 50% |
| 6+ | 100% |

So a quiet read endpoint sits at 10%, a write endpoint starts at 25% before
anything has gone wrong, and a rare write that has already produced a mismatch
is at 100%.

**Rare** means the endpoint's share of eligible requests is below 2%, counted
once at least 200 requests have been seen. The minimum stops every endpoint
from looking rare in the first seconds after a restart. The 2% line is set so
a 1-in-100 endpoint counts as rare and a 1-in-20 endpoint does not.

Those traffic counts age out. They are kept in two buckets and rotated once
per risk window, and the share is taken over both, so an endpoint that was
busy an hour ago doesn't go on looking busy forever.

An endpoint is `METHOD /path`. The query string is not part of it, so
`/api/products?category=audio` and `/api/products` share one score. They are
the same handler, and splitting on the query would give every distinct query
its own score and its own traffic share.

**The window** is the last 10 minutes of stored comparisons (`RISK_WINDOW`),
re-read every 5 seconds (`RISK_REFRESH`).

### Where the signals come from

The gateway can't score an endpoint from its own memory, because it never sees
the comparison result. That happens downstream in the worker. So the gateway
reads them back:

```text
worker writes comparisons -> Postgres
                               |
            every 5s, grouped by method and path
                               v
                      gateway sampler (in memory)
```

Request counts for rarity are the gateway's own, counted per endpoint as
requests arrive.

This means risk mode starts cold. After a restart it has no history, so every
read endpoint is at 10% and writes are at 25% until the first results come
back. That is a real cost of the design and the experiment measures it.

### Explaining a rate

```bash
curl -s http://localhost:8081/internal/sampling | python -m json.tool
```

```json
{
  "endpoint": "POST /api/orders",
  "requests": 3,
  "share": 0.047,
  "score": 5,
  "rate": 0.5,
  "reasons": ["+3 recent mismatch", "+2 write request"],
  "signal": {"mismatches": 10, "errors": 0, "slow": 0}
}
```

The dashboard shows the same table. `SAMPLING_RATE` is ignored in risk mode
and reported as 0, because the rate is per endpoint rather than global.

### What it doesn't handle yet

An endpoint is the exact path, so a service with ids in the path gets one
score per id: `/api/products/1` and `/api/products/2` are scored separately
rather than as one `/api/products/{id}` endpoint. For the test services that
is 12 products and it is fine. For a real API it means two things: a mismatch
on one id doesn't raise the rate for the others, and the sampler holds a key
per id seen in the current window. Grouping by route template is the fix, and
the gateway doesn't know the templates today.

If Postgres is unreachable the refresh fails, the warning is logged, and the
last known signals stay in place, so sampling keeps working at the rates it
already had.

### What it deliberately isn't

No machine learning, no decay curve, no tuning loop. The rules are a fixed
table that can be read in ten seconds and argued with. If a weight is wrong
it is one number in one file, and the experiment can be re-run.
