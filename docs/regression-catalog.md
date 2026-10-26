# Regression catalog

The candidate service (`services/candidate/main.go`) is a copy of the stable
service with a fixed set of faults built into it. They are the ground truth
for every experiment: when a run reports "4 of 5 detected", these are the five
it means.

Each fault has an ID that does not change, so results from different weeks can
be compared. Every one is marked with a `REGRESSION (Rn)` comment in the
candidate source.

| ID | Type | Request | Stable v1 | Candidate v2 | Expected outcome |
|---|---|---|---|---|---|
| R1 | response value | `GET /api/products/12` | `price: 49.99` | `price: 59.99` | `different`, diff `price: 49.99 != 59.99` |
| R2 | HTTP error | `GET /api/products/8` | `200` | `500 inventory lookup failed` | `error`, diff `status: 200 != 500` |
| R3 | latency | `GET /api/products?category=audio` | ~1 ms | ~400 ms slower | `match` with `candidate_slow` set |
| R4 | timeout | `GET /api/users/7` | ~1 ms | hangs 3 s, past the 2 s shadow timeout | `error`, `timed out after 2s` |
| R5 | write behaviour | `POST /api/orders` (body below) | `total = subtotal + shipping` = 184.99 | shipping left out: `total` = 179.99 | `different`, diff `total: 184.99 != 179.99` |

## Why these five

R1 to R4 cover the four ways a deployment usually goes wrong: wrong data, a
new error, a slowdown, and a hang. R5 was added in week 7 because every fault
before it was a `GET`. A write request is the case where shadow traffic is
actually dangerous, so the catalog needed one.

The list is deliberately short. More faults would make the detection rate look
more precise than it is, and each one has to stay hand-verifiable.

The R5 request is fixed, because the product it orders matters:

```bash
curl -X POST http://localhost:8080/api/orders   -H 'Content-Type: application/json'   -d '{"customer":"sam@example.com","items":[{"product_id":4,"quantity":1}]}'
```

Product 4 costs the same in both versions, so the only difference in the
response is the one R5 causes. Ordering product 12 instead gives two
differences, `subtotal` and `total`, because R1 changes that product's price
and the wrong price flows into the order. That is realistic but it makes R5
harder to measure, so the fixed workload orders product 4.

## Detection rules

A regression counts as detected when at least one stored comparison for that
request matches the rule below. These are the same conditions the experiment
harness queries for.

| ID | Detected when |
|---|---|
| R1 | a row with `path = /api/products/12` and `outcome = different` |
| R2 | a row with `path = /api/products/8` and `outcome = error` |
| R3 | a row with `path = /api/products`, `query = category=audio` and `candidate_slow = true` |
| R4 | a row with `path = /api/users/7` and `outcome = error` |
| R5 | a row with `path = /api/orders`, `method = POST` and `outcome = different` |

A regression that is never mirrored is never detected. That is the whole point
of the sampling experiment, and it is why detection is measured per run rather
than assumed.

## How often each fault appears

The faults are the same whichever experiment is running. What changes is how
often the workload sends the request that triggers them:

| Workload | Fault frequency | Why |
|---|---|---|
| `experiments/midterm.js` | R1 about 10% of requests, R2-R4 about 5%, R5 1% | week 7, where everything was found every time |
| `experiments/rare.js` | each fault once in 1000 requests | week 9, where sampling actually starts missing them |

Keeping rarity in the workload rather than in the candidate service means the
catalog below describes both experiments, and the detection rules never
change.

## Controls

Three requests exist to catch false positives. If any of these ever comes out
as `different`, the comparison rules are wrong, not the candidate.

| Request | Why it is here |
|---|---|
| `GET /api/users/3` | same data, different key order and indentation |
| `GET /api/products/99` | 404 from both versions |
| `POST /api/orders` fields `order_id`, `created_at`, `payment_reference` | different on every request by design; covered by the ignore-field rules |
| `GET /health` | the two versions report `v1` and `v2` on purpose, so the `version` field is ignored too |

## R5 in detail

`POST /api/orders` is the only write in the system and it is sandboxed. The
stable service writes to the `stable_app` database and calls the live endpoint
of the mock payment service. The candidate writes to `candidate_sandbox` and
calls the sandbox endpoint, and it rejects any write that does not carry the
`X-MirrorGate-Shadow` header. Details are in `docs/shadow-writes.md`.

The response contains three fields that are different on every single request
(`order_id`, `created_at`, `payment_reference`). Without ignore rules every
order would be reported as a difference and R5 would be buried in noise, so
the comparison rules in `docs/comparison-rules.md` list them as ignored.
