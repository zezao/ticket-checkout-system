# Ticket Checkout System

A Go ticket-checkout backend where PostgreSQL alone decides who owns a seat. The
repository implements Phases 1 to 3: the foundation, the seat reservation
lifecycle, and payments with an idempotent settlement worker and an in-process
mock provider. Customers browse events, hold a seat, pay through the mock, and
receive a confirmed order. Redis, benchmarks and deployment arrive in Phases 4
to 6.

## Architecture

```text
                        HTTP
  client ────────────▶ api (net/http) ────────────▶ PostgreSQL
                       read, hold, payment          events, seats, holds,
                       handlers                     payment_attempts, orders,
                            │                       outbox
                            │ app ports
                            ▼
                       postgres adapter
                            ▲
  worker ──▶ settler ───────┤
         ├─▶ sweeper ───────┤
         ├─▶ invariant ─────┘
         │   checker
         └──▶ mockpay provider ────────────────▶ PostgreSQL (mockpay schema)
```

The API and worker are separate binaries built from one Go module. PostgreSQL is
the source of truth. The mock provider runs inside the worker with its own
connection pool and its own schema, so its idempotency survives worker restarts.
The worker settles payments, sweeps expired holds and checks data invariants. The
outbox relay and Redis cache arrive in Phase 4.

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

Compose starts PostgreSQL, applies `migrations/0001_init.up.sql` and
`migrations/0002_mockpay.up.sql`, loads the development seed, then starts the API
on port 8080 and the worker. The seed includes one event with eight seats. The
default `MOCK_BEHAVIOR` is `success`.

```sh
BASE=http://localhost:8080
EVENT_ID=0199a000-0000-7000-8000-000000000001
SEAT_ID=0199a000-0000-7000-8000-000000000201
USER_ID=0199a000-0000-7000-8000-000000000901

curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/events
curl http://localhost:8080/events/$EVENT_ID/seats
```

Take a hold, pay, and poll:

```sh
HOLD=$(curl -sS -X POST "$BASE/events/$EVENT_ID/holds" \
  -H "X-User-ID: $USER_ID" -H "Content-Type: application/json" \
  -d "{\"seat_id\":\"$SEAT_ID\"}")
HOLD_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["hold"]["id"])' <<< "$HOLD")

curl -sS -X POST "$BASE/holds/$HOLD_ID/payment" \
  -H "X-User-ID: $USER_ID" -H "Idempotency-Key: demo-key-1"

curl -sS -H "X-User-ID: $USER_ID" "$BASE/holds/$HOLD_ID"
```

The worker settles the attempt within about a second. Poll `GET /holds/{id}`
until `hold.status` is `confirmed` or `payment_failed`; the response carries the
hold and payment as separate objects. The same idempotency key returns the same
attempt; a different key after the payment is final returns 409.

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
required; everything else has a default. Compose forwards `HOLD_TTL`,
`PAYMENT_DEADLINE`, `PAYMENT_LEASE`, `PROVIDER_TIMEOUT`, `SWEEP_INTERVAL` and
`MOCK_BEHAVIOR`, so manual fault demos need no file edits.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | none | PostgreSQL connection string |
| `REDIS_URL` | empty | Redis; empty disables caching and rate limiting (Phase 4) |
| `HTTP_ADDR` | `:8080` | API listen address |
| `HOLD_TTL` | `10m` | Lifetime of an active hold |
| `PAYMENT_DEADLINE` | `15m` | When a pending payment counts as overdue and alerts |
| `PAYMENT_LEASE` | `30s` | How long a claimed attempt is left alone by other workers |
| `PROVIDER_TIMEOUT` | `10s` | Provider request timeout; keep it well below the lease |
| `SWEEP_INTERVAL` | `5s` | Expiry sweeper period |
| `PG_MAX_CONNS` | `25` | pgx pool size |
| `MOCK_BEHAVIOR` | `success` | `success`, `decline`, `timeout_after_charge` or `unresolved` |

## HTTP API

| Method and path | Purpose | Success | Errors |
| --- | --- | --- | --- |
| `GET /healthz` | Liveness | 200 | none |
| `GET /readyz` | Readiness; PostgreSQL required | 200 | 503 |
| `GET /events` | List events | 200 | none |
| `GET /events/{id}` | Event detail | 200 | 404 |
| `GET /events/{id}/seats` | Seat map | 200 | 404 |
| `POST /events/{id}/holds` | Hold one seat; body `{"seat_id": "..."}` | 201 new, 200 if the caller already holds it | 401, 400, 404, 409 `seat_unavailable` |
| `GET /holds/{id}` | Hold and payment status (the polling target) | 200 | 401, 404 |
| `DELETE /holds/{id}` | Release an active hold | 204 | 401, 404, 409 `hold_not_active` |
| `POST /holds/{id}/payment` | Start payment; requires `Idempotency-Key` | 202 open, 200 final | 400 `idempotency_key_required`, 401, 404, 409 `hold_not_active`, 409 `payment_in_progress`, 409 `idempotency_key_reused` |

The hold endpoints require a valid `X-User-ID` header. A hold that belongs to
someone else returns 404, never 403, so ids do not leak.

Hold responses look like:

```json
{
  "hold": {
    "id": "0199a000-0000-7000-8000-000000000801",
    "status": "payment_pending",
    "seat_id": "0199a000-0000-7000-8000-000000000101",
    "expires_at": "2027-06-12T19:40:00Z"
  },
  "payment": {
    "id": "0199a000-0000-7000-8000-000000000802",
    "status": "pending"
  }
}
```

`payment` is null until payment starts. A `payment_pending` hold with a
`created` or `pending` payment means the provider has not given a final answer;
keep polling.

Conventions:

- JSON bodies; money as integer cents plus a currency code; times in RFC 3339 UTC.
- Errors use `{"error": {"code": "...", "message": "..."}}`.
- Every response carries an `X-Request-ID`. A valid incoming value is preserved;
  otherwise one is generated. Requests are logged as structured JSON with that id.
- Unknown events, seats, holds, and malformed ids return 404 `not_found`.
- Idempotency keys are unique per user, not globally. Reusing a key on another
  hold returns 409 `idempotency_key_reused`.

## Hold lifecycle

```text
                 start payment            settle
  active ─────────────────────▶ payment_pending ──────▶ confirmed
    │  │                                └── decline ──▶ payment_failed
    │  └── release ──▶ released
    └───── expire ───▶ expired
```

- `active` to `payment_pending` requires the owner, an active hold and an
  unexpired hold.
- `active` to `released` requires the owner.
- `active` to `expired` happens only after `expires_at`, through the sweeper or
  lazily during acquire.
- `payment_pending` to `confirmed` or `payment_failed` happens only through
  settlement, guarded by the attempt status.
- Nothing leaves `confirmed`, `expired`, `released`, or `payment_failed`. A new
  request creates a new hold row; hold rows are never deleted.

Expiry uses `clock_timestamp()`, so the database decides when a hold ends.
Acquire also retires an expired hold before claiming the seat, which means the
sweeper is tidiness rather than a correctness dependency. While a payment is in
flight, `seats.hold_expires_at` is NULL, so the expiry predicate can never match.

## Payment lifecycle

```text
  created ──succeeded──▶ succeeded
     │  └──declined────▶ failed
     └──timeout / processing / unknown──▶ pending ── ... ──▶ succeeded or failed
```

- One hold produces at most one attempt, enforced by `UNIQUE (hold_id)`. A
  declined hold cannot be retried; the customer takes a new hold.
- The provider idempotency key is the attempt id. Re-issuing `CreateCharge` is
  safe, which is why a worker crash before settlement recovers cleanly.
- A timeout or 5xx means **unknown**, not failed. The attempt becomes `pending`,
  the seat stays held, and the worker keeps checking.
- Only a definitive decline releases the seat.
- Settlement is one transaction: attempt, hold, seat and order update together,
  guarded by the attempt status. The order is inserted with `INSERT ... SELECT`
  from the attempt and hold, never from application parameters.
- The worker claims due attempts with `FOR UPDATE SKIP LOCKED` and a lease, calls
  the provider outside any transaction, retries with 2s-to-1m exponential backoff
  with jitter, and uses a fixed 10-minute interval after `deadline_at`.
- An attempt still open past `deadline_at` is overdue: the worker logs
  `payments overdue` every tick and keeps the seat held. v1 has no operator
  tooling; follow `docs/runbooks/manual-payment-resolution.md`.
- Prometheus metrics for payments and overdue counts arrive in Phase 4.

The mock provider stores one row per idempotency key in `mockpay.charges`:

| `MOCK_BEHAVIOR` | First `CreateCharge` | Later calls |
| --- | --- | --- |
| `success` | records `succeeded`, returns it | return the existing charge |
| `decline` | records `declined`, returns it | return the existing charge |
| `timeout_after_charge` | records `succeeded`, returns a timeout | return the existing charge |
| `unresolved` | records `processing`, returns it | return `processing` forever |

A key the provider has never seen answers `unknown`.

## Concurrency and invariants

- One transaction holds the seat with a conditional `UPDATE`; the second racer
  waits on the row lock, re-evaluates, and gets `seat_unavailable`.
- `holds_one_live_per_seat` is a partial unique index over live and sold holds.
  It is the backstop even if a code path misses a guard.
- Expired active holds are reclaimed lazily by acquire and in batches by the
  sweeper. `payment_pending` seats have `hold_expires_at = NULL` and are never
  swept.
- Each acquire, release, sweep and settlement writes its outbox rows in the same
  transaction. The relay and Redis invalidation arrive in Phase 4.
- The domain transition functions and the SQL guards are compared for every hold
  status pair by an agreement test.
- Settlement retries are idempotent: a final attempt rejects another result, and
  `UNIQUE (seat_id)` on orders means a seat is sold at most once.

## Testing

```sh
make test-unit   # unit tests and fuzz seeds, no Docker
make test        # go test -race ./..., including Testcontainers
make lint
```

`make test` starts a throwaway PostgreSQL 17 container through Testcontainers,
applies the real migrations, and exercises the API, use cases and worker:

- read API and readiness
- hold acquire, duplicate acquire, release, ownership isolation
- expiry sweeper; invariants 2 and 5
- domain/SQL hold transition agreement for all 36 status pairs
- C1: 1,000 concurrent holds on one seat give one winner
- C2: 10,000 requests across 100 seats give exactly 100 holds
- C3: randomized hold/release/pay/expire traffic with the worker running
- success, decline, timeout-after-charge and unresolved payment paths
- invariants 4, 6, 7, 8 and 9, fault cases 1-5, 7-11, and worker restart
  recovery

See `docs/testing.md` for a hands-on playbook.

Run the transition fuzzer for longer with:

```sh
go test -fuzz=FuzzHoldTransitions -fuzztime=30s ./internal/domain/reservation
```

## Project layout

```text
cmd/
  api/                  HTTP process wiring
  worker/               settlement, sweeper and invariant checker wiring
internal/
  domain/               pure types and transition rules; standard library only
    reservation/        Seat, Hold, statuses, and legal transitions
    payment/            Payment attempt statuses and outcomes
    order/              orders (read model)
  app/                  use cases, backoff, and ports
  adapters/
    httpapi/            routing, middleware, read/hold/payment handlers
    postgres/           pgx pool, stores, settlement transaction, invariants
    mockpay/            in-process provider backed by the mockpay schema
    redis/              cache and rate limiting (Phase 4)
  worker/               settlement, sweep and invariant loops
  config/               environment configuration
migrations/             numbered golang-migrate files
migrations/seed/        local development data
test/integration/       Testcontainers integration, concurrency, and fault suites
docs/adr/               architecture decision records
docs/runbooks/          operational procedures
deploy/                 infrastructure as code (Phase 6)
```

## Architecture decisions

- [0001: PostgreSQL owns seat state](docs/adr/0001-postgresql-owns-seat-state.md)
- [0002: Where invariants live](docs/adr/0002-where-invariants-live.md)
- [0003: Payment state machine](docs/adr/0003-payment-state-machine.md)
- [Manual payment resolution](docs/runbooks/manual-payment-resolution.md)

## Roadmap

- [x] Phase 1: foundation, schema, migrations, read API, CI
- [x] Phase 2: seat reservation, expiry sweeper, outbox writes, invariant checker, ADRs
- [x] Phase 3: payments, settlement worker, mock provider, fault injection
- [ ] Phase 4: Redis cache, rate limiting, metrics, tracing
- [ ] Phase 5: benchmarks
- [ ] Phase 6: AWS deployment

The implementation spec is at `docs/spec/ticket-checkout-system.md`.
