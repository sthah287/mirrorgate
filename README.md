# MirrorGate

A shadow traffic system for testing a new version of a backend service with real requests before it gets released.

MirrorGate sits in front of a service. Every request goes to the current **stable** version and the client gets that response like normal. At the same time, MirrorGate sends a copy of the request to the **candidate** version, the two responses get compared, and the result is saved. The candidate's response is never returned to the client.

Progress Report 1 was the first working version, where the gateway did the comparison itself. Progress Report 2 moved that work out of the gateway and onto a Kafka topic, added tracing across the services, and added traffic sampling. Week 7 made the loose parts concrete: a fixed catalog of the faults being tested, written-down comparison rules with configurable ignored fields, safe shadowing of `POST` requests, risk-priority sampling, and a repeated experiment instead of one run per strategy.

The documents that define the experiment are in `docs/`:

| | |
|---|---|
| [docs/regression-catalog.md](docs/regression-catalog.md) | the five faults (R1-R5) and what counts as detecting each one |
| [docs/comparison-rules.md](docs/comparison-rules.md) | what counts as a difference |
| [docs/sampling-strategies.md](docs/sampling-strategies.md) | the four strategies, including how the risk score is calculated |
| [docs/shadow-writes.md](docs/shadow-writes.md) | how a mirrored `POST` is kept from causing real side effects |

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
                    |             |  writes go to separate databases, and
                    |             |  outbound calls to separate endpoints
                    |             v
                    |      mock payment / email (:9003)
                    |        live        sandbox
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

Added in week 7, for the midterm:

- **A fixed regression catalog**, R1 to R5, with stable IDs and a written rule for what counts as detecting each one. The faults are marked `REGRESSION (Rn)` in the candidate source
- **Comparison rules that are written down and configurable.** Fields that are different on every request (`order_id`, `created_at`, `payment_reference`) can be ignored with `COMPARE_IGNORE_FIELDS`, and the latency rule has its own setting
- **Safe `POST` shadowing.** `POST /api/orders` is mirrored. The candidate writes to its own database, calls sandbox endpoints on the mock payment and email services, and refuses any write that is not shadow traffic
- **Risk-priority sampling**, a fourth strategy that scores each endpoint from recent results and raises its rate, with the score and the reasons visible on the dashboard and the internal API
- **A repeated experiment**: 5 trials per strategy on the same workload and the same container CPU and memory limits, with means and standard deviations

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
│   ├── candidate/            v2 test service with the R1-R5 faults
│   └── mock-external/        stand-in payment and email services
├── dashboard/                React + TypeScript (Vite)
├── db/
│   ├── init-app-databases.sql  stable_app and candidate_sandbox
│   └── init.sql                comparison schema
├── docs/                     catalog, comparison rules, sampling, shadow writes
├── experiments/
│   ├── sampling.js           week 5 k6 workload
│   ├── run.sh                week 5 experiment, one run per strategy
│   ├── midterm.js            week 7 workload, adds the order POST
│   ├── run-midterm.sh        week 7 experiment, 5 trials per strategy
│   ├── collect*.py           merge k6 + gateway + database results
│   └── summarize*.py         build summary.csv and summary.md
├── results/progress-report-2/  week 5 experiment output
├── results/midterm/            week 7 experiment output
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
| Mock payment / email | http://localhost:9003/calls |
| Redpanda (Kafka API) | localhost:19092 |
| Postgres | localhost:5433 (user/password/db: `mirrorgate`) |

Postgres is on 5433 instead of 5432 so it doesn't conflict with a local install. Redpanda advertises `redpanda:9092` inside the compose network and `localhost:19092` outside it.

Everything in `db/` runs only when the Postgres volume is first created, and that is also where the `stable_app` and `candidate_sandbox` databases come from. Coming from an older volume, reset it:

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
| `MAX_SHADOW_IN_FLIGHT` | `100` | extra requests aren't mirrored |
| `MIRROR_METHODS` | `GET,POST` | comma separated |
| `SAMPLING_MODE` | `full` | `full`, `random`, `endpoint`, or `risk` |
| `SAMPLING_RATE` | `1` | 0 to 1, used by `random` and as the `endpoint` fallback. Ignored in `risk` mode |
| `SAMPLING_RULES` | | per-path rates for `endpoint` mode |
| `RISK_WINDOW` | `10m` | how far back `risk` mode looks for mismatches |
| `RISK_REFRESH` | `5s` | how often it re-reads them from Postgres |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | empty turns tracing off |
| `STABLE_VERSION` / `CANDIDATE_VERSION` | `v1` / `v2` | labels shown on the dashboard |

Worker configuration: `DATABASE_URL`, `KAFKA_BROKERS`, `KAFKA_TOPIC`, `KAFKA_GROUP`, `OTEL_EXPORTER_OTLP_ENDPOINT`, plus the comparison rules `COMPARE_IGNORE_FIELDS` and `COMPARE_SLOW_THRESHOLD_MS`.

The gateway, stable, and candidate containers have fixed CPU and memory limits in `docker-compose.yml` (2 CPUs and 1 GB for the gateway, 1 CPU and 512 MB each for the services). They are there so every experiment run gets the same budget.

## Sampling

Mirroring every request doubles the load on the backend. Sampling is the part of the project that asks how much of that is actually necessary, so the rate is configurable and the decision is made before the gateway does any shadow work at all: a skipped request costs one rate lookup and nothing else. No request copy, no goroutine, no candidate call.

```bash
# mirror everything (default)
SAMPLING_MODE=full docker compose up -d gateway

# raise the rate on endpoints that have recently gone wrong
SAMPLING_MODE=risk docker compose up -d gateway

# mirror a quarter of eligible requests
SAMPLING_MODE=random SAMPLING_RATE=0.25 docker compose up -d gateway

# different rates per endpoint, SAMPLING_RATE is the fallback for anything unmatched
SAMPLING_MODE=endpoint SAMPLING_RATE=0 \
  SAMPLING_RULES='/api/products*=0.5,/api/users*=0.25,/health=0' \
  docker compose up -d gateway
```

`risk` mode works out its own rate per endpoint, between 10% and 100%, from recent mismatches, errors, slowness, how rare the endpoint is, and whether it is a write. The full table is in [docs/sampling-strategies.md](docs/sampling-strategies.md), and `GET /internal/sampling` shows the current score and the reasons behind it for every endpoint.

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

The candidate service has five faults in it on purpose. They are listed in [docs/regression-catalog.md](docs/regression-catalog.md) and marked `REGRESSION (Rn)` in `services/candidate/main.go` and `services/candidate/orders.go`:

| | Request | Stable v1 | Candidate v2 | MirrorGate result |
|---|---|---|---|---|
| R1 | `GET /api/products/12` | 200, price 49.99 | 200, price **59.99** | `different` (`price: 49.99 != 59.99`) |
| R2 | `GET /api/products/8` | 200 | **500** | `error` |
| R3 | `GET /api/products?category=audio` | ~1ms | **~400ms** | `match` + `SLOW` |
| R4 | `GET /api/users/7` | ~1ms | **hangs 3s** | `error` (`timed out after 2s`) |
| R5 | `POST /api/orders` | total 184.99 | total **179.99**, shipping left out | `different` (`total: 184.99 != 179.99`) |
| | `GET /api/products/1` | 200 | 200, same body | `match` |
| | `GET /api/users/3` | 200 | 200, same data but different key order and indentation | `match` |
| | `GET /api/products/99` | 404 | 404 | `match` |

R5 is a write, so it is the one that needs the sandbox:

```bash
curl -X POST http://localhost:8080/api/orders \
  -H 'Content-Type: application/json' \
  -d '{"customer":"sam@example.com","items":[{"product_id":4,"quantity":1}]}'

# one real order, one sandbox order, one live charge, one sandbox charge
curl -s http://localhost:9001/internal/orders   # stable_app
curl -s http://localhost:9002/internal/orders   # candidate_sandbox
curl -s http://localhost:9003/calls             # what the mock services were asked to do
```

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

## The week 5 sampling experiment

The question is: if MirrorGate mirrors less traffic, does it still catch the regressions, and does it cost less? This section is the week 5 version, kept because the midterm experiment below builds on it.

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

## The midterm experiment

Week 5 ran each strategy once. This one runs each of six strategies five
times on the same workload, with the same container CPU and memory limits and
the same reset before every run. What each number means and what is held
constant is in [docs/evaluation-measures.md](docs/evaluation-measures.md).

```bash
docker compose up -d
./experiments/run-midterm.sh          # 30 runs, about half an hour
python experiments/chart_midterm.py
```

The workload is `experiments/midterm.js`: 3000 requests at a constant 100 per
second, in a fixed sequence, carrying the five faults from the catalog. One
request in a hundred is the order `POST`, so R5 is deliberately the rare one.

Results are in `results/midterm/`, one JSON per run plus `summary.csv`,
`summary.md`, and `strategies.png`.

### Cost

| Strategy | Mirrored | Actual rate | Regressions found |
|---|---|---|---|
| full (100%) | 3000 / 3000 | 100.0% (sd 0.0) | 5.0 of 5 |
| random 50% | 1510 / 3000 | 50.3% (sd 0.9) | 5.0 of 5 |
| random 25% | 745 / 3001 | 24.8% (sd 0.4) | 5.0 of 5 |
| random 10% | 313 / 3001 | 10.4% (sd 0.3) | 5.0 of 5 |
| endpoint rules | 1268 / 3000 | 42.3% (sd 1.0) | 5.0 of 5 |
| risk-priority | 406 / 3000 | 13.5% (sd 0.6) | 5.0 of 5 |

### Detection speed, in requests until the fault was first caught

| Strategy | R1 price | R2 500 | R3 slow | R4 timeout | R5 order total |
|---|---|---|---|---|---|
| full (100%) | 11 | 13 | 14 | 15 | 0 |
| random 50% | 18 ± 11 | 20 ± 11 | 33 ± 36 | 22 ± 10 | 139 ± 113 |
| random 25% | 20 ± 12 | 34 ± 17 | 35 ± 24 | 72 ± 37 | 357 ± 320 |
| random 10% | 181 ± 221 | 171 ± 84 | 95 ± 71 | 305 ± 235 | 1190 ± 469 |
| endpoint rules | 11 ± 1 | 24 ± 26 | 33 ± 20 | 86 ± 51 | 159 ± 229 |
| risk-priority | 83 ± 61 | 88 ± 61 | 218 ± 148 | 94 ± 74 | 355 ± 133 |

### What this run actually shows

**Every strategy found all five faults, every time.** Detection rate is 100%
across the board, including random 10%. At 3000 requests even the rarest
fault is hit 30 times, so a one-in-ten sample lands on it eventually. The
detection rate stopped being the interesting measure and detection speed took
over. Making the faults rare enough to actually be missed is the next
experiment.

**Client latency is flat.** p50 sits between 0.53 and 0.61 ms and p95 between
0.96 and 1.03 ms across every strategy, and the differences are inside the
run-to-run spread. That is the expected result rather than a surprise: the
candidate call and the publish both happen after the client already has its
response.

**Risk-priority costs about what random 10% costs, and is only clearly better
in one place.** It mirrored 13.5% against random 10%'s 10.4%. On R5, the rare
write endpoint, it needed 355 ± 133 requests against 1190 ± 469, so roughly a
third of the traffic for the same catch. On R1 through R4 the two overlap
inside their standard deviations, so this run does not separate them there.
The one place it is clearly behind is against random 25%, which catches
everything except R5 faster, because risk-priority starts every run cold: with
no history, every read endpoint sits at the 10% floor until something has
already gone wrong once.

So the honest summary is that the scoring buys early attention on write and
rare endpoints, which is what the rules were written for, and buys nothing
measurable on busy read endpoints that random sampling hits anyway.

**Shadow write isolation held in all 30 runs.** Every run checks that live
payment calls equal the number of orders the client sent, and that sandbox
payment calls equal the number of orders mirrored. It is in each run file as
`side_effects.isolation_ok`.

**The run found a noisy field.** The first full run reported 475 differences
when the faults only explain 327. The other 148 were all `/health`: the two
services report `"version":"v1"` and `"v2"` on purpose. `version` went onto
`COMPARE_IGNORE_FIELDS` and the experiment was re-run from scratch.

### The week 5 experiment

`./experiments/run.sh` and `results/progress-report-2/` are still here. That
one is a single run per strategy on a workload without the write endpoint, and
it is what the earlier table below describes.

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
- comparison rules: ignored fields are skipped by name and by path, a real change in the same body is still caught, the latency threshold is configurable, and bad settings are rejected
- risk-priority scoring: a quiet endpoint stays at the floor, a recent mismatch or candidate error raises the rate, writes and rare endpoints are prioritised, the worst case reaches 100%, traffic counts age out, and the reasons shown on the dashboard match the score
- shadow writes: the client gets the stable order, the candidate's write lands only in the sandbox database, the mock records exactly one live charge and one sandbox charge, the candidate refuses a write without the shadow header, and a failed order doesn't affect the client
- end to end against the real containers: all demo scenarios come out with the right outcome, fields that differ by design aren't reported, the client always gets the stable body while the stored candidate body differs, and the gateway counters agree with what's in Postgres

## Current limitations

- `POST /api/orders` is the only write that exists. `PUT`, `PATCH`, and `DELETE` are allowed by `MIRROR_METHODS` and scored as writes, but no endpoint uses them, so that path is untested
- The sandbox is two databases in one Postgres container and a mock payment service. It proves the candidate writes nowhere the users can see, but pointing a candidate at a real provider's sandbox is a configuration promise, and a wrong environment variable would still be a real charge
- **Publishing is at-most-once.** There is no outbox or retry, so a broker outage can lose comparisons. The client is never affected, and the failures are counted, but the count is an upper bound rather than an exact loss number
- **Consuming is at-least-once.** Offsets are committed after the save, so an event can be processed twice. `SaveComparison` is idempotent on `request_id` to absorb that
- Responses are fully buffered in memory, so very large or streaming responses aren't a good fit yet
- Ignored fields are matched by exact name or exact path. No wildcards, no regular expressions
- Risk mode starts cold: after a restart it has no history, so it cannot prioritise an endpoint until something has already gone wrong there once. It can also starve itself, since an endpoint sampled at 10% may never produce the mismatch that would raise its rate. The rare-endpoint rule is what partly covers this
- Risk scores are keyed by exact path, so `/api/products/1` and `/api/products/2` are separate endpoints. Grouping by route template is the fix
- Risk signals are polled from Postgres every 5 seconds, so the rate reacts a few seconds after the worker stores a result. The rarity counts are the gateway's own and reset when it restarts
- JSON numbers are compared as float64
- Sampling counters are in memory, so they reset when the gateway restarts. Skipped requests are counted but not recorded anywhere durable
- One partition on the topic and one worker. Nothing has been tested with a consumer group bigger than one
- The experiment is 5 trials per strategy on one laptop. Five faults is a small sample, so a detection rate of 80% means four out of five, not a precise probability
- CPU and memory are not measured. The containers have fixed limits instead, so no strategy can use more than another
- The internal API has no authentication
- The stable/candidate services are simple test services with hardcoded data

## Next steps

- An outbox so a broker outage can't lose comparisons, instead of counting failures and hoping
- A workload where the regressions are rare enough that sampling actually starts missing them, so the strategies separate more than they do at 3000 requests
- Request replay: record traffic and run it against a candidate later, instead of only mirroring live
- Fault injection on the candidate rather than faults hardcoded in its source
- More than one partition and more than one worker, to see whether the comparison stage actually scales out
- Kubernetes deployment
