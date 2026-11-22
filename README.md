# Ticket Checkout System

A Go ticket-checkout backend where PostgreSQL alone decides who owns a seat. The
project is built in phases; this repository currently contains the Phase 1
foundation: the module layout, PostgreSQL schema and migrations, configuration,
structured logging, health checks, and the read API.

Phase 1 does not implement holds, payments, or the worker loops yet. Those
arrive in Phases 2 and 3, and the full invariants and concurrency tests land
with them.

## Architecture

```text
                    HTTP                         PostgreSQL
  client ─────────▶ api (net/http) ─────────────▶  events, seats,
                    internal/adapters/httpapi      migrations, seed
                           │
                           │ app ports
                           ▼
                    internal/adapters/postgres

  worker (stub) ────────────────────────────────▶  will own settlement,
                                                    sweeper, outbox, checker
```

The API and worker are separate binaries built from one Go module. PostgreSQL is
the source of truth. Redis, the payment provider, and the background loops are
added in later phases.

Layering follows ports and adapters:

- `internal/domain` has no I/O and imports only the standard library.
- `internal/app` holds use cases and the ports they depend on.
- `internal/adapters` implement the ports. Only the PostgreSQL adapter writes SQL.
- `cmd` wires adapters and starts a process.
- `.golangci.yml` enforces the import direction with `depguard`.

## Prerequisites

- Go 1.27.3, installed with [asdf](https://asdf-vm.com/) (pinned in `.tool-versions`)
- Docker with Docker Compose (for PostgreSQL and the integration tests)
- `golangci-lint` 2.15.0, installed with asdf (pinned in `.tool-versions`)

```sh
asdf plugin add golang
asdf plugin add golangci-lint
asdf install
```

## Quick start

```sh
docker compose up --build
```

Compose starts PostgreSQL, applies `migrations/0001_init.up.sql` through
golang-migrate, loads the development seed from `migrations/seed/`, then starts
the API on port 8080 and the worker. The seed includes one event with eight
seats.

```sh
EVENT_ID=0199a000-0000-7000-8000-000000000001

curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/events
curl http://localhost:8080/events/$EVENT_ID
curl http://localhost:8080/events/$EVENT_ID/seats
```

To stop the stack and remove its volumes:

```sh
docker compose down -v
```

### Running the API outside Compose

Start only PostgreSQL and the one-off migration/seed services, then run the API
with the local connection string the Makefile provides:

```sh
docker compose up -d postgres migrate seed
make run-api
```

## Configuration

The API and worker read the following environment variables. `DATABASE_URL` is
required; everything else has a default.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | none | PostgreSQL connection string |
| `REDIS_URL` | empty | Redis; empty disables caching and rate limiting (Phase 4) |
| `HTTP_ADDR` | `:8080` | API listen address |
| `HOLD_TTL` | `10m` | Lifetime of an active hold (Phase 2) |
| `PAYMENT_DEADLINE` | `15m` | When a pending payment counts as overdue (Phase 3) |
| `PAYMENT_LEASE` | `30s` | Payment claim lease (Phase 3) |
| `PROVIDER_TIMEOUT` | `10s` | Provider request timeout (Phase 3) |
| `SWEEP_INTERVAL` | `5s` | Expiry sweeper period (Phase 2) |
| `PG_MAX_CONNS` | `25` | pgx pool size |
| `MOCK_BEHAVIOR` | `success` | Mock provider behavior (Phase 3) |

## HTTP API

Implemented in Phase 1:

| Method and path | Purpose | Success | Errors |
| --- | --- | --- | --- |
| `GET /healthz` | Liveness | 200 | none |
| `GET /readyz` | Readiness; PostgreSQL required | 200 | 503 |
| `GET /events` | List events | 200 | none |
| `GET /events/{id}` | Event detail | 200 | 404 |
| `GET /events/{id}/seats` | Seat map | 200 | 404 |

Hold, release, payment, and polling endpoints arrive in Phases 2 and 3.

Conventions:

- JSON bodies; money as integer cents plus a currency code; times in RFC 3339 UTC.
- Errors use `{"error": {"code": "...", "message": "..."}}`.
- Every response carries an `X-Request-ID`. A valid incoming value is preserved;
  otherwise one is generated. Requests are logged as structured JSON with that id.
- Unknown events and malformed event ids both return 404 `not_found`.

## Project layout

```text
cmd/
  api/                  HTTP process wiring
  worker/               background process wiring (stub in Phase 1)
internal/
  domain/               pure types; standard library only
    reservation/        Seat and seat status
    payment/            payment domain (Phase 3)
    order/              orders (Phase 3)
  app/                  use cases and ports
  adapters/
    httpapi/            routing, middleware, handlers
    postgres/           pgx pool and read stores
    redis/              cache and rate limiting (Phase 4)
    mockpay/            mock provider (Phase 3)
  worker/               background loops (Phase 2+)
  config/               environment configuration
migrations/             numbered golang-migrate files
migrations/seed/        local development data
test/integration/       Testcontainers integration suite
docs/                   specification and ADRs
deploy/                 infrastructure as code (Phase 6)
```

## Testing

```sh
make test-unit   # unit tests, no Docker
make test        # go test -race ./..., including Testcontainers
make lint
```

`make test` starts a throwaway PostgreSQL 17 container through Testcontainers,
applies the real migrations, and exercises the HTTP API. Docker must be running.
`go test -race ./...` is the canonical command used in CI.

## Roadmap

- [x] Phase 1: foundation, schema, migrations, read API, CI
- [ ] Phase 2: seat reservation, expiry sweeper, outbox, invariant checker, ADRs
- [ ] Phase 3: payments, settlement worker, mock provider, fault injection
- [ ] Phase 4: Redis cache, rate limiting, metrics, tracing
- [ ] Phase 5: benchmarks
- [ ] Phase 6: AWS deployment

The implementation spec is at `docs/spec/ticket-checkout-system.md`.
