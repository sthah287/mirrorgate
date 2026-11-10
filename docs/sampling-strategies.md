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

An endpoint is `METHOD /route`. The query string is not part of it, so
`/api/products?category=audio` and `/api/products` share one score. They are
the same handler, and splitting on the query would give every distinct query
its own score and its own traffic share.

**The window** is the last 10 minutes of stored comparisons (`RISK_WINDOW`),
re-read every 5 seconds (`RISK_REFRESH`).

### Route templates

`RISK_ROUTE_TEMPLATES=true` reduces the path to the route behind it before it
is scored, so `/api/products/1` and `/api/products/2` are both
`GET /api/products/{id}`. **It is off by default**, which is a measurement
rather than a preference; the numbers are at the end of this section. A segment is replaced when it looks like an
identifier: all digits, a UUID, or a long opaque string with a digit in it.
Anything else is left alone, so `/api/products` keeps its own score and is
never folded in with `/api/products/{id}`, and `/api/v2/products` stays as it
is. Methods stay separate, so `GET /api/orders/{id}` and
`DELETE /api/orders/{id}` are still two endpoints.

The comparison records keep the original path. Only the sampler's key
changes, so every stored row, every dashboard list and every result file
still says which product was asked for.

The risk signals are read out of Postgres grouped by the stored path, which
means several rows can belong to one route. Their counts are added together
rather than the last one winning.

This is a trade rather than an improvement:

- **For**: a mismatch found on one id raises the rate for every id behind the
  same handler, which is what a deployment wants when the bug is in the
  handler. The sampler also stops holding a key per id, which on a real API
  is unbounded.
- **Against**: an id that is a tiny share of traffic on its own no longer
  counts as rare. Grouped, the ids add up to a busy route, so the +2 rare
  bonus that per-url scoring gave each individual product goes away.

What the grouping does to the rate table is in
`results/pr4/templates-summary.md`: five endpoints scored instead of twenty
on the experiment's workload. What it does to detection is in
`results/pr4/rare-random/`, over 20 trials with the fault positions
randomised:

| | templates on | templates off |
|---|---|---|
| traffic mirrored | 20.9% | 10.2% |
| faults caught | 53% | 60% |
| mirrored requests per fault caught | 236 | 102 |

Paired by trial the difference in detection is -7 points with a 95% interval
of -17 to +5, so the two are not separated; the difference in cost is not
close. Grouping doubled the traffic MirrorGate mirrored and bought nothing
measurable on this workload.

The mechanism is the one described above, running the other way: a single
mismatch anywhere behind `GET /api/products/{id}` lifts the rate for a route
carrying three quarters of the traffic, and the ids that carry the faults
stop counting as rare. Turn it on when the id space is large enough that
scoring every url would hold an unbounded number of keys, which is a real
problem this workload does not have.

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

Templating is a guess at the route rather than the route itself. The gateway
does not read the services' route tables, so a path segment that is a word
rather than an id stays as it is even when it is a parameter, and a service
whose ids are short words would not be grouped. Reading the real routes from
the service would be better and is not something the gateway can do today.

If Postgres is unreachable the refresh fails, the warning is logged, and the
last known signals stay in place, so sampling keeps working at the rates it
already had.

### What it deliberately isn't

No machine learning, no decay curve, no tuning loop. The rules are a fixed
table that can be read in ten seconds and argued with. If a weight is wrong
it is one number in one file, and the experiment can be re-run.
