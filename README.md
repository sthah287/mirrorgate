# MirrorGate

A shadow traffic system for testing a new version of a backend service with real requests before it gets released.

MirrorGate sits in front of a service. Every request goes to the current **stable** version and the client gets that response like normal. At the same time, MirrorGate sends a copy of the request to the **candidate** version, the two responses get compared, and the result is saved. The candidate's response is never returned to the client.

Progress Report 1 was the first working version, where the gateway did the comparison itself. Progress Report 2 moved that work out of the gateway and onto a Kafka topic, added tracing across the services, and added traffic sampling so the project can start measuring how much mirrored traffic is actually needed to catch a regression.

## Architecture

```text
                        Client
                          |
                          v
                 MirrorGate gateway (:8080)
                    |             |
                    |             |  copy of the request (separate goroutine,
                    v             v  own 2s timeout, only if sampling says yes)
              Stable v1 (:9001)  Candidate v2 (:9002)
                    |             |
                    v             |
            response to client    |
                                  v
                          comparison event
                                  |
                                  v
                    Redpanda topic mirrorgate.comparisons
                                  |
                                  v
                        Comparison worker
                                  |
                                  v
                             PostgreSQL
                                  |
                                  v
              internal API (:8081) <---- React dashboard (:3000)

  every service also sends spans to Jaeger (:16686)
```

The gateway still owns the request path: it forwards to stable, returns that response, and fires the shadow request. What it no longer does is compare the two responses or write to the database. It publishes one event with both responses in it and moves on. The comparison worker consumes that topic, classifies the result, and stores it.

## What works right now

From Progress Report 1:

- Go gateway that forwards any request to the stable service, keeping the method, path, query string, and headers (hop-by-hop headers are removed)
- Shadow copy of `GET` requests sent to the candidate in the background. The client response never waits for it
- Separate timeout for shadow requests (`SHADOW_TIMEOUT`, default `2s`) and a cap on how many can be in flight at once (`MAX_SHADOW_IN_FLIGHT`)
- Response comparison: status code, JSON bodies compared as parsed values so key order and whitespace don't count, field-level differences like `price: 49.99 != 59.99`, candidate errors, and a slow flag when the candidate is more than 200ms behind
- Results saved to PostgreSQL (`request_comparisons`), served by an internal API on a separate port
- React + TypeScript dashboard with summary numbers, a filterable table, and a detail view with both response bodies

Added in Progress Report 2:

- **Redpanda** as the event broker. The gateway publishes one comparison event per mirrored request to `mirrorgate.comparisons`
- **Comparison worker**, a separate Go service that consumes those events, runs the comparison, and writes to Postgres. Offsets are committed only after a successful save, so a failed write is retried instead of dropped
- **Traffic sampling** with three modes (`full`, `random`, `endpoint`), decided before any shadow work starts
- **OpenTelemetry tracing** across the gateway, both test services, and the worker, with the trace context carried through Kafka message headers. One request is one trace, end to end
- Trace IDs stored with each comparison and linked from the dashboard detail view
- Sampling panel on the dashboard: mode, target rate, mirrored vs skipped, and publish failures
- A k6 experiment that runs the same workload against each sampling strategy and writes real results to `results/`
- A failure test script for the two new failure modes (worker down, broker down)

## Project structure

```text
mirrorgate/
├── gateway/                  Go gateway
│   ├── cmd/mirrorgate/       main: config, servers, shutdown
│   ├── internal/proxy/       forwarding + shadow requests + publishing
│   ├── internal/sampling/    which requests get mirrored
│   └── internal/api/         internal API for the dashboard
├── comparison-worker/        Go consumer
│   ├── cmd/worker/           main: Kafka + Postgres wiring
│   └── internal/consume/     event -> comparison -> stored row
├── shared/                   used by both services
│   ├── event/                the comparison event
│   ├── broker/               Kafka producer, consumer, trace headers
│   ├── compare/              response comparison
│   ├── storage/              Postgres queries
│   └── tracing/              OTLP setup
├── services/
│   ├── stable/               v1 test service
│   └── candidate/            v2 test service with intentional regressions
├── dashboard/                React + TypeScript (Vite)
├── db/init.sql               schema
├── experiments/
│   ├── sampling.js           k6 workload
│   ├── run.sh                runs every strategy
│   ├── collect.py            merges k6 + gateway + database results
│   └── summarize.py          builds summary.csv
├── results/progress-report-2/  actual experiment output
├── tests/                    end-to-end tests against the running stack
├── scripts/
│   ├── demo.sh               one request per scenario + results table
│   ├── traffic.sh            repeated mixed traffic for the dashboard
│   ├── failure-test.sh       worker down / broker down
│   └── run-tests.sh          runs every module's tests
└── docker-compose.yml
```

The repo is a few small Go modules rather than one. `shared/` is a module the gateway and the worker both depend on with a `replace` directive, which is what stops the comparison logic from being copy-pasted into two services. Unit tests sit next to the code they test; `tests/` holds the ones that need the whole stack running.

## Running it

Requirements: Docker Desktop. Go, Node, and k6 are only needed if you want to run things outside Docker.

```bash
docker compose up --build
```

| What | URL |
|---|---|
| Gateway (send traffic here) | http://localhost:8080 |
| Internal API | http://localhost:8081/internal/stats |
| Dashboard | http://localhost:3000 |
| Jaeger (traces) | http://localhost:16686 |
| Stable service directly | http://localhost:9001 |
| Candidate service directly | http://localhost:9002 |
| Redpanda (Kafka API) | localhost:19092 |
| Postgres | localhost:5433 (user/password/db: `mirrorgate`) |

Postgres is on 5433 instead of 5432 so it doesn't conflict with a local install. Redpanda advertises `redpanda:9092` inside the compose network and `localhost:19092` outside it.

The schema in `db/init.sql` only runs when the Postgres volume is created, and PR2 added a `trace_id` column. If you are coming from the PR1 volume, reset it:

```bash
docker compose down -v
docker compose up --build
```

### Gateway configuration

| Variable | Default | |
|---|---|---|
| `STABLE_URL` | `http://localhost:9001` | |
| `CANDIDATE_URL` | `http://localhost:9002` | |
| `DATABASE_URL` | `postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable` | read only in the gateway now |
| `KAFKA_BROKERS` | `localhost:19092` | comma separated |
| `KAFKA_TOPIC` | `mirrorgate.comparisons` | |
| `PORT` / `ADMIN_PORT` | `8080` / `8081` | proxy / internal API |
| `SHADOW_TIMEOUT` | `2s` | max time for a candidate request |
| `MIRROR_METHODS` | `GET` | comma separated |
| `MAX_SHADOW_IN_FLIGHT` | `100` | extra requests aren't mirrored |
| `SAMPLING_MODE` | `full` | `full`, `random`, or `endpoint` |
| `SAMPLING_RATE` | `1` | 0 to 1, used by `random` and as the `endpoint` fallback |
| `SAMPLING_RULES` | | per-path rates for `endpoint` mode |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | empty turns tracing off |
| `STABLE_VERSION` / `CANDIDATE_VERSION` | `v1` / `v2` | labels shown on the dashboard |

Worker configuration: `DATABASE_URL`, `KAFKA_BROKERS`, `KAFKA_TOPIC`, `KAFKA_GROUP`, `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Sampling

Mirroring every request doubles the load on the backend. Sampling is the part of the project that asks how much of that is actually necessary, so the rate is configurable and the decision is made before the gateway does any shadow work at all: a skipped request costs one rate lookup and nothing else. No request copy, no goroutine, no candidate call.

```bash
# mirror everything (default)
SAMPLING_MODE=full docker compose up -d gateway

# mirror a quarter of eligible requests
SAMPLING_MODE=random SAMPLING_RATE=0.25 docker compose up -d gateway

# different rates per endpoint, SAMPLING_RATE is the fallback for anything unmatched
SAMPLING_MODE=endpoint SAMPLING_RATE=0 \
  SAMPLING_RULES='/api/products*=0.5,/api/users*=0.25,/health=0' \
  docker compose up -d gateway
```

A rule pattern is either an exact path or a prefix ending in `*`, and the first matching rule wins, so put the specific ones first. Watch the slash: `/api/products/*` matches `/api/products/12` but **not** `/api/products`, so a rule set written that way silently stops mirroring the collection endpoint. The experiment below caught exactly that.

The counters are on the internal API and the dashboard:

```bash
curl -s http://localhost:8081/internal/stats | python -m json.tool
```

```json
"sampling": { "mode": "random", "rate": 0.25, "mirrored": 702, "skipped": 2299 },
"gateway":  { "eligible": 3001, "mirrored": 702, "published": 702, "publish_failed": 0 }
```

Skipped requests are counted, not stored. There is no row and no response body kept for traffic that wasn't mirrored, so the count is cheap. Because the counters live in the gateway's memory, restarting the gateway resets them, which is what the experiment relies on to get a clean number per run.

## Tracing

Every service sends spans to Jaeger over OTLP/HTTP. Send a request and look it up:

```bash
curl http://localhost:8080/api/products/12
```

Open http://localhost:16686, pick **gateway** under Service, and hit Find Traces. One request looks like this:

```text
gateway   GET                                 <- the incoming request
gateway     GET stable-service:9001
stable-service  stable-service
gateway     GET candidate-service:9002
candidate-service  candidate-service
gateway     publish comparison event
comparison-worker   process comparison        <- through Kafka
comparison-worker     save comparison
```

The gateway to worker link is the interesting one, because Kafka isn't an HTTP call. The trace context is written into the message headers by the producer and read back out by the consumer (`shared/broker/trace.go`). The trace ID is also stored on the comparison row, so the dashboard detail view links straight to the trace.

Tracing is off when `OTEL_EXPORTER_OTLP_ENDPOINT` is empty, which keeps `go test` and running a service by hand from needing a collector.

## Demo endpoints

The candidate service has a few regressions on purpose (search for `REGRESSION` in `services/candidate/main.go`):

| Request | Stable v1 | Candidate v2 | MirrorGate result |
|---|---|---|---|
| `GET /api/products/1` | 200 | 200, same body | `match` |
| `GET /api/products/12` | 200, price 49.99 | 200, price **59.99** | `different` (`price: 49.99 != 59.99`) |
| `GET /api/products/8` | 200 | **500** | `error` |
| `GET /api/products?category=audio` | ~1ms | **~400ms** | `match` + `SLOW` |
| `GET /api/users/7` | ~1ms | **hangs 3s** | `error` (`timed out after 2s`) |
| `GET /api/users/3` | 200 | 200, same data but different key order and indentation | `match` |
| `GET /api/products/99` | 404 | 404 | `match` |

In every case the client gets the stable response:

```bash
curl -i http://localhost:8080/api/products/12
```

Run all scenarios and print what was stored:

```bash
./scripts/demo.sh --reset
```

The comparisons now arrive a moment after the client response, because they go through the broker first. `demo.sh` waits for the rows instead of reading the table straight away.

Generate more traffic for the dashboard:

```bash
./scripts/traffic.sh 10
```

On Windows run the scripts from Git Bash.

## The sampling experiment

The question is: if MirrorGate mirrors less traffic, does it still catch the regressions, and does it cost less?

```bash
docker compose up -d
./experiments/run.sh
```

For each strategy the script restarts the gateway with that configuration, clears the comparisons table, runs the same k6 workload, waits for the worker to drain the topic, and then reads the k6 metrics, the gateway counters, and the database into one result file. Results land in `results/progress-report-2/`, one JSON per strategy plus `summary.csv`.

The workload is a fixed 20-slot cycle at 100 requests/second for 30 seconds (3000 requests), so every strategy sees the same requests in the same proportions rather than a different random draw. Half of it is ordinary product reads; the rest includes all four known regressions. The mix is documented at the top of `experiments/sampling.js`.

One strategy at a time, or a different load:

```bash
./experiments/run.sh random-25
RATE=500 DURATION=20s SUFFIX=-load ./experiments/run.sh full
```

Results from the run committed in `results/`:

| Strategy | Mirrored | Actual rate | Stored | Regressions found | p50 | p95 | p99 |
|---|---|---|---|---|---|---|---|
| off (0%) | 0 / 3001 | 0.0% | 0 | 0 of 4 | 0.67 ms | 1.12 ms | 1.56 ms |
| full (100%) | 3000 / 3000 | 100.0% | 3000 | 4 of 4 | 0.74 ms | 1.03 ms | 1.60 ms |
| random 50% | 1527 / 3000 | 50.9% | 1527 | 4 of 4 | 0.68 ms | 1.02 ms | 1.49 ms |
| random 25% | 702 / 3001 | 23.4% | 702 | 4 of 4 | 0.63 ms | 0.97 ms | 1.56 ms |
| random 10% | 299 / 3000 | 10.0% | 299 | 4 of 4 | 0.63 ms | 0.98 ms | 1.38 ms |
| endpoint | 1243 / 3001 | 41.4% | 1243 | **3 of 4** | 0.71 ms | 1.08 ms | 1.56 ms |
| endpoint (fixed rules) | 1279 / 3001 | 42.6% | 1279 | 4 of 4 | 0.68 ms | 1.01 ms | 1.50 ms |

What this run actually shows:

- Down to 10% sampling, all four regressions were still found. At 3000 requests the regression endpoints are hit often enough that a 1-in-10 sample still lands on each of them. Sampling reduced the stored comparisons by 90% without losing any of the four.
- Client latency is basically flat across strategies, and the differences are smaller than the run-to-run noise. That is the expected result rather than a surprise: the candidate request and the publish both happen after the client already has its response, so mirroring is not on the critical path.
- The `endpoint` row missing a regression was not sampling working as intended. The rules were `/api/products/*=0.5` with a 0% fallback, and `/api/products?category=audio` has the path `/api/products`, which the pattern doesn't match. The slow-response regression was never mirrored once. The `endpoint (fixed rules)` row is the same intent with `/api/products*` and it finds all four.

A separate pair of runs at 500 requests/second for 20 seconds (10001 requests each, no dropped iterations, `results/progress-report-2/overhead/`) is where the cost of mirroring becomes measurable:

| | p50 | p95 | p99 |
|---|---|---|---|
| no mirroring | 0.50 ms | 0.78 ms | 1.26 ms |
| 100% mirroring | 0.58 ms | 0.92 ms | 1.77 ms |

So about 0.1 ms at the median and 0.5 ms at p99 for mirroring everything. Small, but it is the direction you would expect, and it only showed up once the load was high enough for the shadow goroutines to compete for the same CPU.

This is an early experiment on one laptop, with one repeat per configuration. It is enough to show the harness works and to give a baseline, not enough to put error bars on anything.

## Failure scenarios

```bash
./scripts/failure-test.sh
```

**The comparison worker stops.** Clients keep getting stable responses, the gateway keeps publishing, and the events sit in the topic. When the worker comes back it reads from its last committed offset and catches up. This is the main reason the broker is in the design, and it is the one scenario that behaves cleanly.

**The broker stops.** Clients still get stable responses, which is the property that matters, and the gateway counts the publish failures instead of failing the request. What happens to those events is not reliable. MirrorGate has no outbox and does not retry, so anything the Kafka client can't deliver is the client's business: in one run 9 of 10 events sat in its internal batch and were flushed once traffic resumed, and in another run with no traffic after recovery all of them were dropped. The `publish_failed` counter is therefore an upper bound on losses, not a count of them. Fixing this properly needs an outbox, and it is on the list below.

## Tests

Without Go installed, against the running stack:

```bash
docker compose up -d
docker compose run --rm tests
```

With Go installed:

```bash
./scripts/run-tests.sh                                 # every module
TEST_GATEWAY_URL=http://localhost:8080 \
TEST_ADMIN_URL=http://localhost:8081 ./scripts/run-tests.sh   # including tests/
```

The `tests/` module talks to the running containers over HTTP and skips itself when those two variables aren't set, so `go test ./...` still works with nothing running.

What's covered:

- stable response (status, headers, body) is returned to the client, and a different candidate response never reaches it
- a candidate that times out or is down doesn't slow down or break the client response
- a broker that rejects every publish doesn't affect the client either, and the failure is counted
- the request body is sent to both services when a method is mirrored, and methods outside `MIRROR_METHODS` aren't mirrored
- a request skipped by sampling never reaches the candidate at all
- 100% sampling mirrors everything, 0% mirrors nothing, a seeded generator gives repeatable decisions, endpoint rules override the fallback rate, and bad configuration is rejected instead of defaulted
- the comparison event survives a JSON round trip, and the trace context survives Kafka headers
- the worker classifies every one of the demo scenarios correctly from an event, and a failed save is returned so the offset isn't committed
- end to end against the real containers: all seven demo scenarios come out with the right outcome, the client always gets the stable body while the stored candidate body differs, and the gateway counters agree with what's in Postgres

## Current limitations

- Only `GET` is mirrored by default. `POST`/`PUT`/`DELETE` can be enabled, but nothing stops the candidate from causing real side effects
- **Publishing is at-most-once.** There is no outbox or retry, so a broker outage can lose comparisons. The client is never affected, and the failures are counted, but the count is an upper bound rather than an exact loss number
- **Consuming is at-least-once.** Offsets are committed after the save, so an event can be processed twice. `SaveComparison` is idempotent on `request_id` to absorb that
- Responses are fully buffered in memory, so very large or streaming responses aren't a good fit yet
- No way to ignore fields that are always different, like timestamps or generated IDs
- JSON numbers are compared as float64
- Sampling counters are in memory, so they reset when the gateway restarts. Skipped requests are counted but not recorded anywhere durable
- One partition on the topic and one worker. Nothing has been tested with a consumer group bigger than one
- The experiment is one run per configuration on a laptop, with no repeats
- The internal API has no authentication
- The stable/candidate services are simple test services with hardcoded data

## Next steps

- An outbox so a broker outage can't lose comparisons, instead of counting failures and hoping
- Handling `POST`/`PUT` safely (side-effect isolation)
- Ignore rules for noisy fields like timestamps and generated IDs
- Record requests and replay them against a candidate later
- More than one partition and more than one worker, to see whether the comparison stage actually scales out
- A fuller experiment: repeated runs, more sampling rates, and a workload where the regression is rare enough that sampling actually starts missing it
- Fault injection on the candidate rather than hardcoded regressions
- Kubernetes deployment
