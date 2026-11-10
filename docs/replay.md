# Recording and replay

Shadow traffic only ever covers what happens to be arriving. If the candidate
was not running when an interesting request came through, or a fix needs
checking against the traffic that found the bug, there is nothing to send at
it. Recording keeps a small sample of read-only traffic so it can be sent at
the candidate later.

Keeping copies of real requests is the risky part of this, so almost all of
the design is about what the feature refuses to do.

## What is recorded

GET requests only, and nothing else ever. That is checked in three places:
the recorder drops anything that is not a GET, the table has
`CHECK (method = 'GET')`, and the runner refuses a row whose method is not
GET before it builds the request. Three checks for one rule is deliberate:
this is the line that decides whether a replay can charge a card.

A row holds the path, the query string, a few headers, and when it was taken.
There is no body, because a GET has none worth keeping, and no response.

Headers are an **allow list**, not a deny list:

```text
Accept  Accept-Language  User-Agent
```

Everything else is dropped, including `Authorization`, `Cookie`,
`X-Api-Key` and anything nobody has thought of yet. A deny list would be one
forgotten header away from storing credentials.

## Limits

| | default | |
|---|---|---|
| `REPLAY_RECORD` | `false` | recording is off unless it is turned on |
| `REPLAY_RECORD_RATE` | `0.05` | share of eligible GETs kept |
| `REPLAY_MAX_RECORDS` | `2000` | hard cap on stored rows |
| `REPLAY_RETENTION` | `1h` | rows older than this are deleted |
| `REPLAY_MAX_URL_BYTES` | `2048` | longer urls are not stored at all |

The cap is enforced inside the insert, so two writers cannot both decide
there is room for the same last row. At the cap the recorder stops rather
than evicting, which keeps a batch made of requests the sampler chose instead
of whatever happened to survive. Retention is a delete on a one minute timer.

Recording never touches the database on the request path. `Observe` applies
the rate, copies the safe headers and hands the request to a bounded queue; a
background writer does the insert. A full queue drops the request and counts
it. The client never waits for any of this.

## Replaying

```bash
curl -s -X POST -H "X-Replay-Token: $REPLAY_TOKEN" \
  "http://localhost:8081/internal/replay?limit=25"
```

```json
{"batch_id":"RBJZNF44T3MF","requested":10,"replayed":10,"failed":0,
 "status_counts":{"200":9,"500":1},"duration_ms":12.6}
```

What the caller can choose is how many requests to replay. That is the whole
input. There is no target in the request, no path, no url: the runner builds
every request from a row it stored itself and sends it to the candidate it
was configured with. **This is not a request-forwarding endpoint**, and it
cannot be turned into one by anything a caller sends.

The rest of the rules:

- **Only the candidate.** The runner holds the candidate's base url and has
  no field for the stable one, so there is no code path that reaches the
  stable service. A test asserts the stable service is called zero times
  during a replay.
- **Nothing is returned to a client.** A replay happens on the internal port,
  outside the request path. The response is drained and discarded; the body
  is never stored, never compared and never sent anywhere.
- **Results are kept apart.** Outcomes go to `replay_results`, not to
  `request_comparisons`. A replay is not a comparison between the two
  services, and mixing the two would change what every earlier measurement
  counted. The table holds the status, the latency, and the error if there
  was one. It has no body column.
- **One batch at a time.** A second trigger while one is running gets 409,
  so the extra load on the candidate stays bounded.
- **Bounded size.** `REPLAY_MAX_BATCH` (default 100) caps a batch whatever
  the caller asks for.

## Protecting the trigger

| | default | |
|---|---|---|
| `REPLAY_TOKEN` | unset | the token the trigger requires; unset means no trigger |
| `REPLAY_MAX_BATCH` | `100` | largest batch, whatever is asked for |
| `REPLAY_TIMEOUT` | `5s` | per replayed request |

The trigger **fails closed**. With no token configured there is nothing to
authorise against and the endpoint answers 503; it is not open, it is absent.
With a token set, a request without it or with the wrong one gets 401 and is
logged, and the comparison is constant time.

The internal API is on its own port, which in a deployment is not published.
It is still behind a token, because the dashboard reaches the same port from
a browser on the developer's machine, so "internal" here means "not routed
from outside", not "only reachable by me". An end-to-end test asserts the
trigger answers 401 or 503 and never 200 without the token, whatever the
gateway is configured with.

## Trying it

```bash
./scripts/replay-demo.sh
```

Turns recording on with a token generated for the run, sends traffic
carrying an `Authorization` header and a `Cookie`, shows what was kept, shows
the trigger refusing an unauthenticated call and a wrong token, runs a real
replay, and checks what it did not touch. It puts the gateway back on the
defaults afterwards.

The run recorded for week 11: 30 GETs recorded and one POST not recorded,
`Accept` and `User-Agent` kept, zero rows mentioning a credential, 401 for
both bad calls, 10 replayed with statuses 200 x9 and 500 x1, the comparison
count unchanged, zero live payment or email calls, and the stable
application's `orders` table unchanged.

The 500 is the candidate's seeded R2 fault answering a replayed
`/api/products/8`, which is the feature doing its job: the fault was found
again without any user traffic hitting it.

## What this does not do

A replay runs inside the request that triggered it. A hundred local
requests take about a hundred milliseconds, so that is fine here, but a
batch against a slow candidate would hold the connection open for as long as
it took, and a caller that gave up would cancel it part way. The outcomes
already collected are still stored, because the insert is detached from the
caller's context, but a real deployment would want the trigger to start the
batch and hand back an id.

It does not compare the replayed response against anything. A replay tells
you the candidate's status and how long it took, not whether the answer was
right; comparing would mean a stable response to compare against, and
re-sending the request to stable is exactly what must not happen. Comparing
against the response stored at record time is possible and is not built.
