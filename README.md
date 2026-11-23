# Ticket Checkout System

A Go ticket-checkout backend where PostgreSQL alone decides who owns a seat. The
repository currently implements Phases 1 and 2: the foundation and the seat
reservation lifecycle. Customers browse events, take a time-limited hold on a
seat, and release it. Payments, the mock provider, Redis, and benchmarks arrive
in Phases 3 to 5.

## Architecture

```text
                        HTTP
  client ────────────▶ api (net/http) ────────────▶ PostgreSQL
                       read + hold handlers          events, seats, holds,
                            │                        payment_attempts, orders,
                            │ app ports              outbox
                            ▼
                       postgres adapter
                            ▲
  worker ──▶ sweeper ───────┤
         └─▶ invariant ─────┘
             checker
```

The API and worker are separate binaries built from one Go module. PostgreSQL is
the source of truth. The worker sweeps expired holds and checks data invariants.
The outbox relay, Redis cache, and payment settlement arrive in later phases.

Layering follows ports and adapters:

- `internal/domain` has no I/O and imports only the standard library.
- `internal/app` holds use cases and the ports they depend on.
- `internal/adapters` implement the ports. Only the PostgreSQL adapter writes SQL.
- `internal/worker` runs the background loops against app ports.
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
SEAT_ID=0199a000-0000-7000-8000-000000000101
USER_ID=0199a000-0000-7000-8000-000000000901

curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/events
curl http://localhost:8080/events/$EVENT_ID/seats
```

Take a hold, read it, and release it:

```sh
curl -X POST http://localhost:8080/events/$EVENT_ID/holds \
  -H "X-User-ID: $USER_ID" \
  -H "Content-Type: application/json" \
  -d "{\"seat_id\":\"$SEAT_ID\"}"

curl -H "X-User-ID: $USER_ID" http://localhost:8080/holds/<hold-id>

curl -X DELETE -H "X-User-ID: $USER_ID" http://localhost:8080/holds/<hold-id>
```

`X-User-ID` is a UUID stub for local development and demos. Never expose it on a
public deployment; anyone could impersonate another user. Replace it with a JWT
middleware without touching the handlers.

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
| `HOLD_TTL` | `10m` | Lifetime of an active hold |
| `PAYMENT_DEADLINE` | `15m` | When a pending payment counts as overdue (Phase 3) |
| `PAYMENT_LEASE` | `30s` | Payment claim lease (Phase 3) |
| `PROVIDER_TIMEOUT` | `10s` | Provider request timeout (Phase 3) |
| `SWEEP_INTERVAL` | `5s` | Expiry sweeper period |
| `PG_MAX_CONNS` | `25` | pgx pool size |
| `MOCK_BEHAVIOR` | `success` | Mock provider behavior (Phase 3) |

## HTTP API

| Method and path | Purpose | Success | Errors |
| --- | --- | --- | --- |
| `GET /healthz` | Liveness | 200 | none |
| `GET /readyz` | Readiness; PostgreSQL required | 200 | 503 |
| `GET /events` | List events | 200 | none |
| `GET /events/{id}` | Event detail | 200 | 404 |
| `GET /events/{id}/seats` | Seat map | 200 | 404 |
| `POST /events/{id}/holds` | Hold one seat; body `{"seat_id": "..."}` | 201 new, 200 if the caller already holds it | 401, 400, 404, 409 `seat_unavailable` |
| `GET /holds/{id}` | Hold and payment status | 200 | 401, 404 |
| `DELETE /holds/{id}` | Release an active hold | 204 | 401, 404, 409 `hold_not_active` |

The hold endpoints require a valid `X-User-ID` header. A hold that belongs to
someone else returns 404, never 403, so ids do not leak. The `payment` object is
null until Phase 3 adds payment attempts.

Hold responses look like:

```json
{
  "hold": {
    "id": "0199a000-0000-7000-8000-000000000801",
    "status": "active",
    "seat_id": "0199a000-0000-7000-8000-000000000101",
    "expires_at": "2027-06-12T19:40:00Z"
  },
  "payment": null
}
```

Conventions:

- JSON bodies; money as integer cents plus a currency code; times in RFC 3339 UTC.
- Errors use `{"error": {"code": "...", "message": "..."}}`.
- Every response carries an `X-Request-ID`. A valid incoming value is preserved;
  otherwise one is generated. Requests are logged as structured JSON with that id.
- Unknown events, seats, holds, and malformed ids return 404 `not_found`.

## Hold lifecycle

```text
                 start payment            settle
  active ─────────────────────▶ payment_pending ──────▶ confirmed
    │  │                                └── decline ──▶ payment_failed
    │  └── release ──▶ released
    └───── expire ───▶ expired
```

- `active` to `payment_pending` requires the owner and an unexpired hold.
- `active` to `released` requires the owner.
- `active` to `expired` happens only after `expires_at`, through the sweeper or
  lazily during acquire.
- `payment_pending` to `confirmed` or `payment_failed` is Phase 3 settlement.
- Nothing leaves `confirmed`, `expired`, `released`, or `payment_failed`. A new
  request creates a new hold row; hold rows are never deleted.

Expiry uses `clock_timestamp()`, so the database decides when a hold ends.
Acquire also retires an expired hold before claiming the seat, which means the
sweeper is tidiness rather than a correctness dependency.

## Concurrency and invariants

- One transaction holds the seat with a conditional `UPDATE`; the second racer
  waits on the row lock, re-evaluates, and gets `seat_unavailable`.
- `holds_one_live_per_seat` is a partial unique index over live and sold holds.
  It is the backstop even if a code path misses a guard.
- Expired active holds are reclaimed lazily by acquire and in batches by the
  sweeper. `payment_pending` seats have `hold_expires_at = NULL` and are never
  swept.
- Each acquire, release, and sweep writes a `seat.changed` row to the outbox in
  the same transaction. The relay and Redis invalidation arrive in Phase 4.
- The domain transition functions and the SQL guards are compared for every
  status pair by an agreement test.

## Testing

```sh
make test-unit   # unit tests and fuzz seeds, no Docker
make test        # go test -race ./..., including Testcontainers
make lint
```

`make test` starts a throwaway PostgreSQL 17 container through Testcontainers,
applies the real migrations, and exercises the API and use cases:

- read API and readiness
- hold acquire, duplicate acquire, release, ownership isolation
- expiry sweeper; invariants 2 and 5
- domain/SQL transition agreement for all 36 status pairs
- C1: 1,000 concurrent holds on one seat give one winner
- C2: 10,000 requests across 100 seats give exactly 100 holds
- fault case 5 (connection cut mid-transaction) and fault case 9 (duplicate
  concurrent acquire from one user)

Run the transition fuzzer for longer with:

```sh
go test -fuzz=FuzzHoldTransitions -fuzztime=30s ./internal/domain/reservation
```

## Project layout

```text
cmd/
  api/                  HTTP process wiring
  worker/               sweeper and invariant checker wiring
internal/
  domain/               pure types and transition rules; standard library only
    reservation/        Seat, Hold, statuses, and legal transitions
    payment/            payment domain (Phase 3)
    order/              orders (Phase 3)
  app/                  use cases and ports
  adapters/
    httpapi/            routing, middleware, read and hold handlers
    postgres/           pgx pool, read stores, atomic hold store, invariant queries
    redis/              cache and rate limiting (Phase 4)
    mockpay/            mock provider (Phase 3)
  worker/               expiry sweeper and invariant checker loops
  config/               environment configuration
migrations/             numbered golang-migrate files
migrations/seed/        local development data
test/integration/       Testcontainers integration, concurrency, and fault suites
docs/adr/               architecture decision records
deploy/                 infrastructure as code (Phase 6)
```

## Architecture decisions

- [0001: PostgreSQL owns seat state](docs/adr/0001-postgresql-owns-seat-state.md)
- [0002: Where invariants live](docs/adr/0002-where-invariants-live.md)

## Roadmap

- [x] Phase 1: foundation, schema, migrations, read API, CI
- [x] Phase 2: seat reservation, expiry sweeper, outbox writes, invariant checker, ADRs
- [ ] Phase 3: payments, settlement worker, mock provider, fault injection
- [ ] Phase 4: Redis cache, rate limiting, metrics, tracing
- [ ] Phase 5: benchmarks
- [ ] Phase 6: AWS deployment

The implementation spec is at `docs/spec/ticket-checkout-system.md`.
