# MirrorGate

A shadow traffic system for testing a new version of a backend service with real requests before it gets released.

MirrorGate sits in front of a service. Every request goes to the current **stable** version and the client gets that response like normal. At the same time, MirrorGate sends a copy of the request to the **candidate** version, compares the two responses, and saves the result. The candidate's response is never returned to the client.

This is the first milestone (Progress Report 1) for my Senior Seminar project.

## Architecture

```text
            Client
              |
              v
     MirrorGate gateway (:8080)
        |               |
        |               |  copy of the request (separate goroutine,
        v               v  own 2s timeout)
  Stable v1 (:9001)   Candidate v2 (:9002)
        |               |
        v               v
  response to client   compare with stable response
                        |
                        v
                   PostgreSQL
                        |
                        v
          internal API (:8081) <---- React dashboard (:3000)
```

## What works right now

- Go gateway that forwards any request to the stable service, keeping the method, path, query string, and headers (hop-by-hop headers are removed)
- Shadow copy of `GET` requests sent to the candidate in the background. The client response never waits for it
- Separate timeout for shadow requests (`SHADOW_TIMEOUT`, default `2s`) and a cap on how many can be in flight at once (`MAX_SHADOW_IN_FLIGHT`)
- Response comparison:
  - status code
  - JSON bodies compared as parsed values, so key order and whitespace don't count as a difference
  - field-level differences like `price: 49.99 != 59.99`
  - candidate errors (5xx, connection failures, timeouts)
  - slow candidate flag when it's more than 200ms slower than stable
- Results saved to PostgreSQL (`request_comparisons` table)
- Internal API for the dashboard on a separate port
- React + TypeScript dashboard with summary numbers, a recent comparisons table, filters, and a detail view with both response bodies
- Docker Compose setup for everything
- Unit tests plus an integration test that uses a real Postgres

## Project structure

```text
mirrorgate/
├── gateway/                  Go gateway
│   ├── cmd/mirrorgate/       main: config, servers, shutdown
│   ├── internal/proxy/       forwarding + shadow requests
│   ├── internal/compare/     response comparison
│   ├── internal/storage/     Postgres queries
│   ├── internal/api/         internal API for the dashboard
│   └── tests/                integration test (needs Postgres)
├── services/
│   ├── stable/               v1 test service
│   └── candidate/            v2 test service with intentional regressions
├── dashboard/                React + TypeScript (Vite)
├── db/init.sql               schema
├── scripts/
│   ├── demo.sh               one request per scenario + results table
│   └── traffic.sh            repeated mixed traffic for the dashboard
└── docker-compose.yml
```

Unit tests are next to the code they test (`proxy_test.go`, `compare_test.go`), which is the normal Go convention. `gateway/tests/` only has the integration test.

## Running it

Requirements: Docker Desktop. Go and Node are only needed if you want to run things outside Docker.

```bash
docker compose up --build
```

| What | URL |
|---|---|
| Gateway (send traffic here) | http://localhost:8080 |
| Internal API | http://localhost:8081/internal/stats |
| Dashboard | http://localhost:3000 |
| Stable service directly | http://localhost:9001 |
| Candidate service directly | http://localhost:9002 |
| Postgres | localhost:5433 (user/password/db: `mirrorgate`) |

Postgres is on 5433 instead of 5432 so it doesn't conflict with a local install.

The schema in `db/init.sql` only runs when the Postgres volume is created. If you change it, reset the volume:

```bash
docker compose down -v
docker compose up --build
```

### Gateway configuration

| Variable | Default | |
|---|---|---|
| `STABLE_URL` | `http://localhost:9001` | |
| `CANDIDATE_URL` | `http://localhost:9002` | |
| `DATABASE_URL` | `postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable` | |
| `PORT` / `ADMIN_PORT` | `8080` / `8081` | proxy / internal API |
| `SHADOW_TIMEOUT` | `2s` | max time for a candidate request |
| `MIRROR_METHODS` | `GET` | comma separated |
| `MAX_SHADOW_IN_FLIGHT` | `100` | extra requests aren't mirrored |
| `STABLE_VERSION` / `CANDIDATE_VERSION` | `v1` / `v2` | labels shown on the dashboard |

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

`GET /api/products` without a category is slow **and** different, because the full list includes product 12.

In every case the client gets the stable response. You can check with:

```bash
curl -i http://localhost:8080/api/products/12
```

Run all scenarios and print what was stored:

```bash
./scripts/demo.sh --reset
```

Generate more traffic for the dashboard:

```bash
./scripts/traffic.sh 10
```

On Windows run the scripts from Git Bash.

## Tests

Without Go installed (runs everything, including the Postgres integration test):

```bash
docker compose up -d postgres
docker compose run --rm gateway-tests
```

With Go installed:

```bash
cd gateway
go test ./...                     # integration test is skipped
TEST_DATABASE_URL="postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable" go test ./...
```

The integration test creates its own temporary schema, so it doesn't touch the demo data.

What's covered:

- stable response (status, headers, body) is returned to the client
- a different candidate response never reaches the client
- a candidate that times out doesn't slow down the client response
- a candidate that is down doesn't break the client response
- the request body is sent to both services when a method is mirrored
- methods not in `MIRROR_METHODS` aren't sent to the candidate
- body differences, status differences, JSON key order/whitespace, nested arrays
- comparisons are saved to Postgres and returned by the internal API

## Current limitations

- Only `GET` is mirrored by default. `POST`/`PUT`/`DELETE` can be enabled, but nothing stops the candidate from causing real side effects (writing to a shared database, charging a card, etc.)
- Responses are fully buffered in memory, so very large or streaming responses aren't a good fit yet
- Every mirrored request is compared and stored. There is no sampling
- No way to ignore fields that are always different, like timestamps or generated IDs
- JSON numbers are compared as float64
- The comparison runs inside the gateway process. If the gateway restarts, in-flight comparisons can be lost (it does wait up to 10s on shutdown)
- The internal API has no authentication
- The stable/candidate services are simple test services with hardcoded data

## Next steps

- Send comparison results through Kafka instead of writing to Postgres from the gateway
- OpenTelemetry tracing across gateway, stable, and candidate
- Sampling strategies (percentage, per-endpoint) and measuring how much traffic is actually needed to catch regressions
- Record requests and replay them against a candidate later
- Handling `POST`/`PUT` safely (side-effect isolation)
- Ignore rules for noisy fields
- Load testing with k6 to measure gateway overhead
- Kubernetes deployment
