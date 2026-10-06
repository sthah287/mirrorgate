# Response comparison rules

What counts as a difference between the stable and candidate responses. The
rules live in `shared/compare/compare.go` and run in the comparison worker,
not in the gateway.

Two settings are configurable on the worker:

| Variable | Default | Meaning |
|---|---|---|
| `COMPARE_IGNORE_FIELDS` | empty | comma separated JSON fields that are allowed to differ |
| `COMPARE_SLOW_THRESHOLD_MS` | `200` | how much slower the candidate may be before it is flagged |

## The rules

**1. Status codes are compared exactly.** `200` against `201` is a difference.
There are no classes or ranges.

**2. JSON bodies are compared structurally.** Both bodies are parsed and
compared as values, so key order, indentation, and whitespace never count.
`{"id":3,"name":"Sam"}` equals `{\n  "name": "Sam",\n  "id": 3\n}`.

**3. Non-JSON bodies are compared as raw bytes,** after trimming leading and
trailing whitespace. The result is one difference, `body: non-JSON bodies
differ`, with no field detail.

**4. Differences are reported per field,** with the path to the field:
`price: 49.99 != 59.99`, `items[1].id: 2 != 3`. A field that only one side has
is reported as `missing in candidate` or `only in candidate`. The list stops
at 20 entries so one badly broken response can't fill the database.

**5. Ignored fields are skipped.** A bare name (`created_at`) ignores that key
at any depth. A dotted path (`payment.reference`) ignores only that one spot.
This exists for values that are different on every request by design:

```
COMPARE_IGNORE_FIELDS=order_id,created_at,payment_reference,version
```

The first three are generated per order. `version` was added after the first
experiment run: `/health` returns `"version":"v1"` from stable and `"v2"` from
the candidate, so every health check in the workload was being reported as a
difference. It is a true difference and a useless one, which is exactly what
this rule is for.

Ignoring is about the field, not the value. If `total` is on the list, a real
pricing bug in `total` is invisible, so the list is kept as short as possible
and lives in `docker-compose.yml` where it can be reviewed.

**6. Arrays are compared in order, element by element.** `[A, B]` against
`[B, A]` is two differences, not a match. If the lengths differ, that is
reported as `items: length 2 != 3` and the shared prefix is still compared.
There is no set matching or key-based matching of elements. For the current
test services the order is stable, so this is good enough; an endpoint with a
genuinely unordered list would need its own rule.

**7. A candidate 5xx while stable is below 500 is an `error`,** not a
difference, even if the bodies happen to match.

**8. A candidate that never answered is an `error`.** Timeouts (past
`SHADOW_TIMEOUT`, default 2 s), connection failures, and unreadable bodies all
land here. There is no status or body to compare, so no field differences are
reported, and the reason is stored in `candidate_error`.

**9. Latency is a flag, not an outcome.** The candidate is marked
`candidate_slow` when

```
candidate_latency - stable_latency > COMPARE_SLOW_THRESHOLD_MS
```

on the same request. A slow but correct response is still a `match` with the
slow flag set. This keeps a slowdown from being confused with a wrong answer,
and it is measured per request rather than as an average, so one slow endpoint
can't be hidden by many fast ones.

## Outcomes

Every comparison ends as exactly one of:

| Outcome | When |
|---|---|
| `match` | same status, no body differences (after ignores). May still be flagged slow |
| `different` | same status but the body differs, or the statuses differ without a candidate 5xx |
| `error` | candidate returned 5xx while stable did not, or the candidate never answered |

## Known limits

- JSON numbers are parsed as float64, so very large integers could lose
  precision. Nothing in the test services goes near that.
- Ignoring is by field name or exact path. There are no wildcards and no
  regular expressions.
- Headers are not compared, only status and body.
