# Route templates: what the grouping changes

3 trials per arm, same workload (`experiments/rare.js`, 3000 requests at
100/s), risk-priority sampling in both. The only difference is
`RISK_ROUTE_TEMPLATES`. Every run file records which setting the gateway
reported, so the two arms cannot be mixed up by a stale image.

## What the sampler is tracking

| | Templates on | Templates off |
| --- | --- | --- |
| Endpoints scored | 5.0 | 20.0 |
| Requests mirrored | 310 | 325 |
| Share of traffic mirrored | 10.33% | 10.82% |
| Gateway CPU (s) | 1.60 | 1.64 |
| Client latency p95 (ms) | 0.90 | 0.89 |

## The rate table, one representative trial each

This is what the gateway was actually scoring at the end of the run,
straight from `/internal/sampling`.

### templates on (risk-t1, 5 endpoints)

| Endpoint | Requests | Share | Score | Rate | Why |
| --- | ---: | ---: | ---: | ---: | --- |
| `POST /api/orders` | 3 | 0.1% | 7 | 100% | +3 recent mismatch, +2 rare endpoint, +2 write request |
| `GET /api/products` | 3 | 0.1% | 3 | 25% | +2 rare endpoint, +1 recent latency regression |
| `GET /api/products/{id}` | 2197 | 73.2% | 0 | 10% | no risk signals |
| `GET /api/users/{id}` | 598 | 19.9% | 0 | 10% | no risk signals |
| `GET /health` | 200 | 6.7% | 0 | 10% | no risk signals |

### templates off (risk-urls-t1, 20 endpoints)

| Endpoint | Requests | Share | Score | Rate | Why |
| --- | ---: | ---: | ---: | ---: | --- |
| `POST /api/orders` | 3 | 0.1% | 7 | 100% | +3 recent mismatch, +2 rare endpoint, +2 write request |
| `GET /api/users/7` | 3 | 0.1% | 2 | 25% | +2 rare endpoint |
| `GET /api/products/12` | 3 | 0.1% | 2 | 25% | +2 rare endpoint |
| `GET /api/products/8` | 3 | 0.1% | 2 | 25% | +2 rare endpoint |
| `GET /api/products` | 3 | 0.1% | 2 | 25% | +2 rare endpoint |
| `GET /health` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/users/3` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/products/7` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/products/9` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/users/2` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/products/11` | 200 | 6.7% | 0 | 10% | no risk signals |
| `GET /api/products/99` | 200 | 6.7% | 0 | 10% | no risk signals |
| ... | | | | | 8 more, all at the 10% floor |

## Detection in these runs

Faults caught, out of 3 trials times five faults. 3 trials cannot
separate two arms whose rates are this close: these counts are here to
show what was run, not to settle which grouping detects more. The
randomised-position experiment with twenty trials per strategy is what
that question needs.

| Arm | Faults caught | R1 | R2 | R3 | R4 | R5 |
| --- | --- | --- | --- | --- | --- | --- |
| templates on | 7 of 15 | 0/3 | 1/3 | 3/3 | 0/3 | 3/3 |
| templates off | 8 of 15 | 2/3 | 1/3 | 0/3 | 2/3 | 3/3 |

## What the numbers mean

Grouping by route is a trade. An endpoint that is one product id out of
many is a tiny share of traffic on its own, so per-URL scoring calls it
rare and lifts its rate; grouped, the ids add up to a busy route and the
rare bonus goes away. In exchange, a mismatch found on one id raises the
rate for every id behind the same handler, which is the behaviour a real
deployment wants when the bug is in the handler rather than in one row.

Which of those matters more depends on where the faults are, and this
workload puts every fault on a fixed, individually rare id.
