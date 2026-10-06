# Safe shadow writes

Mirroring a `GET` is harmless. Mirroring a `POST` is not: if the candidate
shares anything writable with the stable version, shadow traffic changes what
real users see, and every mirrored order would charge the customer twice.

MirrorGate mirrors `POST /api/orders`, so this is how that is made safe.

## What is separated

| | Stable v1 | Candidate v2 |
|---|---|---|
| Database | `stable_app` | `candidate_sandbox` |
| Payments | `POST /payments/charge` (live) | `POST /sandbox/payments/charge` |
| Email | `POST /notifications/email` (live) | `POST /sandbox/notifications/email` |
| Accepts non-shadow writes | yes | **no**, returns 403 |

Both databases live in the same Postgres container, as separate databases with
separate credentials-free connection strings. That is enough for the property
that matters here: no table is shared, so nothing the candidate writes can be
read by the stable service or by a user.

`services/mock-external` stands in for the payment provider and the email
sender. It is a test double. Nothing it does leaves the machine. The live and
sandbox endpoints are separate paths on it, it records every call with the
mode it came in on, and `GET /calls` returns those records, which is how the
tests prove where a side effect went.

## The three guards

**1. Separate configuration.** The candidate container is pointed at the
sandbox database and the sandbox endpoints in `docker-compose.yml`. It has no
credentials for anything live.

**2. The shadow header.** The gateway sets `X-MirrorGate-Shadow: true` on
every mirrored request. The candidate runs with `REQUIRE_SHADOW_HEADER=true`
and refuses any write that doesn't carry it with a 403. So even if something
sent an ordinary order to the candidate by mistake, it would be rejected
rather than quietly written.

**3. The response never leaves.** This is the original MirrorGate property and
it still holds for writes: the client gets the stable response, and the
candidate's order is only ever compared and stored.

## What one mirrored order actually does

```text
POST /api/orders  ->  gateway
                        |
                        +--> stable v1    -> stable_app DB        1 row
                        |                 -> live payment         1 charge
                        |                 -> live email           1 send
                        |                 -> response to client
                        |
                        +--> candidate v2 -> candidate_sandbox DB 1 row (shadow = true)
                                          -> sandbox payment      0 real charges
                                          -> sandbox email        0 real sends
```

Checked by hand on 2026-10-05, one order through the gateway:

```json
"counts": {"live_payment": 1, "live_email": 1, "sandbox_payment": 1, "sandbox_email": 1}
```

One live charge, not two. The sandbox charge is a recorded call against a mock
and its reference is prefixed `pay_sbx_`, so a sandbox payment is obvious even
when looking at a stored response body.

## This is a test environment, not production

The stable service here calls a mock payment provider too, because there is no
real one in this project. In a real deployment the stable side would call the
real provider and only the candidate would be redirected to a sandbox. The
isolation that is actually being demonstrated is:

- the candidate writes to a different database than the one serving users
- the candidate's outbound calls go to different endpoints than the stable ones
- the candidate rejects writes that aren't shadow traffic

## What this does not solve

- A real payment provider's sandbox is their system, not mine. Pointing a
  candidate at it is a configuration promise, and a wrong environment variable
  would still be a real charge.
- The sandbox database is never reset between runs, so it accumulates orders.
  The experiment harness truncates it.
- Reads inside the candidate still come from its own database, so a shadow
  `GET` of an order created by a shadow `POST` would find it. Nothing in the
  current workload does that.
- Only `POST /api/orders` exists. `PUT` and `DELETE` are allowed by
  `MIRROR_METHODS` but no endpoint uses them yet.
