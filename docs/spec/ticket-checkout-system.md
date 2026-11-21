# Ticket Checkout System: Implementation Spec

Nov 4, 2026 · @Jose Baretta

## Overview

Build a Go ticket-checkout backend in which PostgreSQL alone decides who owns a seat, then prove it holds up under concurrency, crashes and payment failures. Customers browse events, hold a seat for a limited time, pay, and receive a confirmed order.

**Goals**

- Exactly one customer can hold a given seat, however many requests arrive at once.
- Every successful charge ends in exactly one confirmed order, and a seat is never charged twice. An unresolved payment never releases the seat automatically: the seat stays held and an alert fires.
- Every state survives a process restart. Process crashes recover automatically. A payment that never resolves needs a person, and v1 only raises an alert.
- The repository shows the reasoning: ADRs, tested invariants, honest benchmarks, and a layered structure that shows where domain rules end and database guarantees begin.

**Non-goals for v1**

- Virtual waiting room, multi-region, Kubernetes, microservices, autoscaling.
- Redis-based seat locking.
- A real payment provider, webhooks, operator tooling and refunds. A mock provider with fault injection stands in; the cut machinery is described under Extensions.
- Multi-seat carts, a seat-map UI, and user accounts beyond a stubbed identity.

**Definition of done**

- `docker compose up` runs the API, worker and PostgreSQL locally (the mock provider runs inside the worker); Redis joins in Phase 4.
- Every invariant in the Invariants section has an automated test.
- `go test -race ./...` passes and integration tests run in CI.
- The README holds the architecture diagram, state machines and ADRs.
- The project is complete after Phase 3. Phases 4 to 6 are extensions.

## Key decisions

PostgreSQL owns seat state, Redis only caches, and payments are a persisted state machine. Each row below becomes a short ADR in `docs/adr/`.

| # | Decision | Choice | Why |
| --- | --- | --- | --- |
| 1 | Who owns a seat | A conditional UPDATE inside a PostgreSQL transaction | Redis replication is asynchronous, so an acknowledged lock can vanish on failover and two users hold one seat |
| 2 | Role of Redis | Seat-map cache and rate limiting only | A stale cache is harmless: the hold endpoint checks PostgreSQL and returns 409 |
| 3 | Time source | The database clock (`clock_timestamp()`) for every expiry | `now()` is frozen at transaction start and application clocks drift between instances |
| 4 | Hold history | Hold rows are never deleted, only change status | Audit trail, easier debugging, and a stable target for the one-live-hold index |
| 5 | Payment outcome | A persisted state machine with idempotency keys; a timeout means unknown, not failed | A charge can succeed after the caller gave up waiting |
| 6 | Ambiguous payments | The seat stays reserved until the provider gives a terminal answer; an overdue payment raises an alert | Releasing early can sell the seat twice |
| 7 | Process model | Modular monolith: one Go module, binaries `api` and `worker` | Shows distributed-systems thinking without microservice overhead |
| 8 | Async propagation | Transactional outbox, polled by the worker | The state change and its event commit together; consumers must be idempotent |
| 9 | Language and HTTP | Go 1.27.x with standard `net/http` routing | Go 1.27.3 was the latest patch in late October 2026 ([source](https://isitpatched.com/go/1.27.3)); Go 1.24 is out of support. Chi or Gin are fine swaps |
| 10 | Build order | docker-compose and Testcontainers first, cloud last | Faster feedback and no idle cloud bill |
| 11 | Code structure | Ports and adapters around a pure domain, with light DDD vocabulary | Practices clean architecture and DDD where they pay off, without mappers between every layer |
| 12 | Where invariants live | Enforced by PostgreSQL, specified by pure domain functions, checked by an agreement test | An in-memory object cannot stop two concurrent requests; the database can |

## Architecture

&#91;embedded content: architecture · 2 processes, 2 data stores, 1 in-process mock provider\]

The API and worker are separate processes built from one Go module. They coordinate through PostgreSQL rows, and both keep working if Redis disappears.

### Layers and dependencies

The code follows ports and adapters, a light form of clean architecture. Inner layers never import outer ones: `domain` knows nothing about use cases, `app` knows nothing about SQL or HTTP, and only `cmd` knows every piece.

&#91;embedded content: layers · 4 packages, imports point toward the domain\]

### Repository layout

```text
ticket-checkout/
├── cmd/
│   ├── api/main.go             # wiring only: config, adapters, use cases, HTTP server
│   └── worker/main.go          # wiring only: config, adapters, worker loops
├── internal/
│   ├── domain/                 # no I/O, standard library imports only
│   │   ├── reservation/        # Seat, Hold, status types and transitions
│   │   ├── payment/            # PaymentAttempt, status types and transitions
│   │   └── order/              # Order
│   ├── app/                    # use cases and the ports they depend on
│   │   ├── ports.go            # stores, TxRunner, Provider, Clock, EventPublisher
│   │   ├── acquire_hold.go     # also release_hold.go, expire_holds.go
│   │   └── settle_payment.go   # also start_payment.go
│   ├── adapters/
│   │   ├── httpapi/            # handlers, middleware, routing
│   │   ├── postgres/           # pgx stores; the atomic SQL from the Core flows section
│   │   ├── redis/              # seat map cache, rate limiter
│   │   └── mockpay/            # in-process mock Provider; state in its own PostgreSQL schema
│   ├── worker/                 # loops: settlement, sweeper, outbox relay, invariant checker
│   └── config/
├── migrations/                 # numbered SQL files, plus seed/
├── test/integration/           # Testcontainers suites and fault injection
├── bench/                      # k6 or oha scripts
├── docs/                       # adr/, benchmarks/
├── deploy/                     # infrastructure as code (Phase 6)
├── docker-compose.yml
└── Dockerfile
```

- `domain` imports only the standard library, so state rules are unit-testable.
- `app` imports `domain` and defines the ports. It contains no SQL, HTTP or Redis code.
- `adapters` implement the ports. Only `adapters/postgres` writes SQL.
- `cmd` wires adapters into use cases and starts the process.
- Enforce the direction with a small test that runs `go list -deps` and fails on a forbidden import.

### Where the rules live

The domain specifies which transitions are legal; PostgreSQL makes the important ones atomic. An in-memory object cannot stop two concurrent requests from both seeing an available seat, so the conditional UPDATE and the partial unique indexes enforce the invariants while the domain functions specify them.

1. **One rule, two forms.** Each transition has a pure function in `domain` and a SQL guard in `adapters/postgres`. A table-driven test runs every status pair through both and requires the same accept or reject result.
2. **Atomic ports.** A port such as `HoldStore.Acquire` documents that it is atomic, and only the PostgreSQL adapter can honor that. Keep an in-memory fake for fast orchestration tests, but never use it for concurrency tests.
3. **A transaction port.** A use case that touches several records, such as settlement, runs inside `TxRunner.Atomically(ctx, func(tx Stores) error)`. The PostgreSQL adapter implements it with one pgx transaction and applies the global, operation-specific lock order (attempt, then hold, then seat; see Core flows) inside itself.
4. **What the domain still owns.** Legal transitions, pricing and validation rules, typed errors, and the names of things.

Write this up as an ADR in Phase 2, next to the PostgreSQL versus Redis decision: why invariants live in the database, and what stays in the domain.

### Contexts, in light DDD

Name code after the business: hold, release, expire, settle, never lock or unlock. Group it into three contexts.

| Context | Owns | Consistency boundary | Invariants |
| --- | --- | --- | --- |
| Reservation | Seat, Hold | A seat and its holds | 1 to 5 |
| Payments | PaymentAttempt | One attempt | 6 and 8 |
| Ordering | Order | One order per seat | 7 |

Invariants 9 and 10 cut across all three contexts.

Settlement updates payment, hold, seat and order in one transaction. Strict DDD would split that across contexts with events and eventual consistency. Version 1 keeps one transaction, because splitting adds intermediate states and failure modes. Record that trade-off in an ADR and revisit it only if the contexts become separate services.

## State machines

&#91;embedded content: hold lifecycle · 6 states, 6 transitions\]

A hold only moves forward, and the seat status always follows the hold status. Payment attempts have four statuses: `created`, `pending`, `succeeded` and `failed`. A payment that never resolves stays `pending`, and its hold stays `payment_pending`, until the provider gives a terminal answer. v1 only raises an alert for that case; the design for handling it is under Extensions.

**Transition guards**

- `active` to `payment_pending`: owner only, and `expires_at` is still in the future.
- `active` to `released`: owner only.
- `active` to `expired`: by the sweeper, or lazily during acquire, and only after `expires_at` has passed.
- `payment_pending` to `confirmed` or `payment_failed`: only through the settlement function, guarded by the attempt status.
- Nothing leaves `confirmed`, `expired`, `released` or `payment_failed`. A new request creates a new hold row.

**Domain types.** Model the statuses as Go string types in `internal/domain`, with one pure function per transition that returns the next status or a typed error. The SQL guards in the Core flows section must agree with these functions, and a table-driven unit test should cover every pair. Add a `testing.F` fuzz test that applies random sequences of operations to these functions and fails if an illegal transition is ever accepted, for example a release after a hold is confirmed.

## Database schema

The schema carries the invariants, so a bug in application code cannot create two live holds for a seat. Use `golang-migrate` and keep this as `migrations/0001_init.up.sql`.

```sql
CREATE TYPE seat_status    AS ENUM ('available', 'held', 'sold');
CREATE TYPE hold_status    AS ENUM ('active', 'payment_pending', 'confirmed',
                                'expired', 'released', 'payment_failed');
CREATE TYPE payment_status AS ENUM ('created', 'pending', 'succeeded', 'failed');

CREATE TABLE events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    venue      text        NOT NULL,
    starts_at  timestamptz NOT NULL,
    currency   char(3)     NOT NULL,          -- ISO 4217; one currency per event
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE seats (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        uuid        NOT NULL REFERENCES events(id),
    section         text        NOT NULL,
    row_label       text        NOT NULL,
    number          int         NOT NULL,
    price_cents     bigint      NOT NULL CHECK (price_cents > 0),
    status          seat_status NOT NULL DEFAULT 'available',
    current_hold_id uuid,         -- FK added below, once holds exists
    hold_expires_at timestamptz,  -- NULL when available, sold or payment_pending
    UNIQUE (event_id, section, row_label, number),
    CHECK ((status = 'available') = (current_hold_id IS NULL)),
    CHECK (status = 'held' OR hold_expires_at IS NULL)
);
CREATE INDEX seats_event_idx ON seats (event_id);

CREATE TABLE holds (
    id           uuid PRIMARY KEY,          -- generated by the API (uuid v7)
    seat_id      uuid        NOT NULL REFERENCES seats(id),
    user_id      uuid        NOT NULL,
    status       hold_status NOT NULL DEFAULT 'active',
    expires_at   timestamptz NOT NULL,
    amount_cents bigint      NOT NULL CHECK (amount_cents > 0),  -- price snapshot at acquire
    currency     char(3)     NOT NULL,                           -- currency snapshot at acquire
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, seat_id)                    -- target of composite foreign keys
);

-- A seat can only point at a hold that belongs to that same seat.
ALTER TABLE seats ADD CONSTRAINT seats_current_hold_matches_seat_fk
    FOREIGN KEY (current_hold_id, id) REFERENCES holds (id, seat_id)
    DEFERRABLE INITIALLY DEFERRED;

-- Invariant 1: at most one live or sold hold per seat
CREATE UNIQUE INDEX holds_one_live_per_seat ON holds (seat_id)
    WHERE status IN ('active', 'payment_pending', 'confirmed');
CREATE INDEX holds_expiry_idx ON holds (expires_at) WHERE status = 'active';

CREATE TABLE payment_attempts (
    id              uuid PRIMARY KEY,
    hold_id         uuid           NOT NULL UNIQUE REFERENCES holds(id),  -- invariant 6: one attempt per hold
    user_id         uuid           NOT NULL,
    idempotency_key text           NOT NULL,        -- client Idempotency-Key header
    amount_cents    bigint         NOT NULL CHECK (amount_cents > 0),  -- copied from the hold
    currency        char(3)        NOT NULL,                           -- copied from the hold
    status          payment_status NOT NULL DEFAULT 'created',
    attempts        int            NOT NULL DEFAULT 0,
    next_check_at   timestamptz    NOT NULL DEFAULT now(),
    deadline_at     timestamptz    NOT NULL,        -- still pending after this: overdue, alert only
    last_error      text,
    traceparent     text,                           -- continues the request trace in the worker
    created_at      timestamptz    NOT NULL DEFAULT now(),
    updated_at      timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key),              -- keys are scoped per user
    UNIQUE (id, hold_id)                            -- target of the orders foreign key
);
CREATE INDEX payments_due_idx ON payment_attempts (next_check_at)
    WHERE status IN ('created', 'pending');

CREATE TABLE orders (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hold_id            uuid        NOT NULL UNIQUE,
    seat_id            uuid        NOT NULL UNIQUE,              -- a seat is sold at most once
    payment_attempt_id uuid        NOT NULL UNIQUE,
    user_id            uuid        NOT NULL,
    amount_cents       bigint      NOT NULL,
    currency           char(3)     NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (hold_id, seat_id) REFERENCES holds (id, seat_id),
    FOREIGN KEY (payment_attempt_id, hold_id) REFERENCES payment_attempts (id, hold_id)
);

CREATE TABLE outbox (
    id            bigserial PRIMARY KEY,
    topic         text        NOT NULL,   -- 'seat.changed', 'order.confirmed'
    payload       jsonb       NOT NULL,
    traceparent   text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    claimed_until timestamptz,            -- lease taken by the relay
    published_at  timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- ================================================================================
-- migrations/0002_mockpay.up.sql: a SEPARATE migration file, shown here for reference.
-- The mock provider's own state, in its own schema so its data stays separate from the application's tables.
-- ================================================================================
CREATE SCHEMA mockpay;
CREATE TABLE mockpay.charges (
    idempotency_key text        PRIMARY KEY,   -- the payment attempt id
    status          text        NOT NULL CHECK (status IN ('succeeded', 'declined', 'processing')),
    amount_cents    bigint      NOT NULL,
    currency        char(3)     NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
```

- A sold seat keeps `current_hold_id` pointing at its confirmed hold, so every sale is traceable. The composite foreign key guarantees that hold belongs to that seat.
- `hold_expires_at` is NULL while a hold is `payment_pending`. The expiry check can then never match, which protects the seat during payment.
- The hold stores `amount_cents` and `currency` when it is acquired, and the payment attempt copies them. A later price change never alters what a customer with an active hold pays. The currency comes from the event.
- A hold has at most one payment attempt (`UNIQUE (hold_id)`). A declined hold cannot be retried; the customer takes a new hold, which keeps every lifecycle unambiguous.
- An order is a self-contained financial record, created with `INSERT ... SELECT` from the attempt and hold rather than from application parameters. It stores amount and currency, and composite foreign keys tie it to a hold that belongs to its seat and an attempt that belongs to its hold.
- Idempotency keys are unique per user, not globally, so one customer's key can never collide with another's or reveal that it exists.
- `outbox.claimed_until` is a lease, so the relay never holds row locks while it runs handlers.
- `mockpay.charges` is keyed by idempotency key and lives in its own schema, so the mock's idempotency survives a worker restart.
- Seeds for local development go in `migrations/seed/`, not in the numbered migrations.

## Invariants

Ten invariants drive the schema, the transactions and the tests. Name each test after its row, for example `TestInvariant06_OneChargePerHold`.

| # | Invariant | Enforced by | Test |
| --- | --- | --- | --- |
| 1 | A seat has at most one live or sold hold | Partial unique index `holds_one_live_per_seat`; conditional UPDATE on acquire | 1,000 concurrent holds on one seat give 1 winner; 10,000 across 100 seats give exactly 100 |
| 2 | A sold seat never returns to available through expiry | Acquire matches only `available`, or `held` with a past `hold_expires_at`; the sweeper touches only `active` holds | Random operation sequences ending in a sweep; sold seats stay sold |
| 3 | Only the hold owner can pay or release | `user_id` in the WHERE clause of every transition | Another user gets 404 on pay and release |
| 4 | A hold cannot start payment after it expires | The `active` to `payment_pending` UPDATE requires `expires_at > clock_timestamp()` | Pay after a 1 s TTL has lapsed gives 409 |
| 5 | A `payment_pending` seat cannot be taken by someone else | Seat `hold_expires_at` is NULL, so the expiry predicate never matches | Acquire during payment gives 409, including after a sweep |
| 6 | One hold produces at most one charge | `UNIQUE (hold_id)` on `payment_attempts`; the provider idempotency key equals the attempt id | 50 concurrent pay calls: the same key always returns the same attempt, a different key gets 409 payment\_in\_progress, and the mock provider records 1 charge |
| 7 | A seat is sold at most once | `UNIQUE (seat_id)` on `orders` | Duplicate settlement attempts and worker retries create 1 order |
| 8 | An ambiguous payment never releases a seat | Timeouts leave the attempt `pending`; only a terminal provider decline releases it | Provider charges then times out; the seat stays held and ends sold |
| 9 | Every transition survives a restart | All state lives in PostgreSQL; workers claim rows with `FOR UPDATE SKIP LOCKED` and a lease | Kill the worker at each injected crash point; the system converges |
| 10 | The cache is never authoritative | The hold and payment paths never read Redis | Stop Redis; holds and payments still work |

The invariant checker in the Background workers section checks the data-level consequences of invariants 1, 2, 5 and 7 after every integration test.

## HTTP API

The API has nine endpoints. Payment confirmation is asynchronous: the client starts a payment, then polls the hold until it reads `confirmed` or `payment_failed`.

The poll response has two objects. `hold` carries the id, status, seat and `expires_at`, which is the hold's own expiry and is never null. `payment` carries the attempt id and status, and is null until payment starts. A `payment_pending` hold with a `created` or `pending` payment means the provider has not given a final answer yet, so the client keeps polling. The seat's own `hold_expires_at` is NULL during payment and the API never exposes it.

```json
{
  "hold": { "id": "...", "status": "payment_pending", "seat_id": "...", "expires_at": "2026-11-19T12:10:00Z" },
  "payment": { "id": "...", "status": "pending" }
}
```

| Method and path | Purpose | Success | Errors |
| --- | --- | --- | --- |
| `GET /healthz` | Liveness | 200 | none |
| `GET /readyz` | Readiness: PostgreSQL required, Redis reported but not fatal | 200 | 503 |
| `GET /events` | List events | 200 | none |
| `GET /events/{id}` | Event detail | 200 | 404 |
| `GET /events/{id}/seats` | Seat map; PostgreSQL only until Phase 4, then Redis cache-aside | 200 | 404 |
| `POST /events/{id}/holds` | Hold one seat; body `{"seat_id": "..."}` | 201 new hold, 200 if the caller already holds that seat | 404 unknown seat, 409 `seat_unavailable`, 429 rate limited |
| `GET /holds/{id}` | Hold and payment status as separate objects (payment is null until payment starts); the polling target | 200 | 404 |
| `DELETE /holds/{id}` | Release an active hold | 204 | 404, 409 `hold_not_active` |
| `POST /holds/{id}/payment` | Start payment; requires an `Idempotency-Key` header | 202 while the payment is open and 200 once it is final; the same key returns the same attempt | 404, 409 `hold_not_active`, 409 `payment_in_progress` for a different key, 409 idempotency\_key\_reused for a key already used on another hold, 400 if the Idempotency-Key header is missing |

**Conventions**

- JSON bodies; money as integer cents plus a currency code; times in RFC 3339 UTC.
- Identity is a stubbed `X-User-ID` header holding a UUID. Replace it with a JWT middleware later without touching handlers. The header is for local development and demos only: never expose it on a public deployment, because anyone could impersonate another user.
- Errors look like `{"error": {"code": "seat_unavailable", "message": "..."}}`.
- A hold that belongs to someone else returns 404, never 403, so ids do not leak.
- Every response carries an `X-Request-ID`, generated if the client sent none.
- `POST /events/{id}/holds` is idempotent by state: asking again for a seat you already hold returns that hold with 200.

## Core flows

Every transition is one short transaction at READ COMMITTED. Never call the payment provider inside a transaction. Each flow below is a use case in the app package, and its SQL runs in the postgres adapter behind a port.

**Lock order.** Locks follow one global order: payment attempt, then hold, then seat. An operation that does not touch an attempt skips that step but never reverses the order. No operation may lock a seat and then try to lock its hold or attempt. When an operation locks several rows of one kind, it locks them in id order. Start payment locks the existing hold and seat, then inserts a new attempt row; it never locks an existing attempt row after the seat, and unique-constraint conflicts are handled by rollback and re-read.

| Operation | Lock order |
| --- | --- |
| Acquire, release, expire | hold, then seat |
| Start payment | hold, then seat, then insert the attempt |
| Settle payment (worker) | attempt, then hold, then seat |

**Row-count rule.** A guard statement that returns zero rows is a normal outcome and maps to a 404 or 409. Every statement after a successful guard must affect exactly the expected rows: one seat for one hold, or the batch size the guard returned in the sweeper. If it does not, roll back, return an internal error and increment `consistency_errors_total`. This applies to acquire, release, start payment, settle payment and expire.

### Acquire a hold

If the caller already has an `active`, unexpired hold on the seat, return it with 200 before opening the transaction. Otherwise:

```sql
BEGIN;

-- 1. Retire an expired hold so the unique index admits the new one.
UPDATE holds SET status = 'expired', updated_at = clock_timestamp()
 WHERE seat_id = $seat AND status = 'active' AND expires_at <= clock_timestamp();

-- 2. Claim the seat. Zero rows: ROLLBACK, then recover (see below).
UPDATE seats s
   SET status = 'held', current_hold_id = $hold,
       hold_expires_at = clock_timestamp() + $ttl
  FROM events e
 WHERE s.id = $seat AND s.event_id = $event AND e.id = s.event_id
   AND (s.status = 'available'
        OR (s.status = 'held' AND s.hold_expires_at <= clock_timestamp()))
RETURNING s.price_cents, e.currency, s.hold_expires_at;

-- 3. Record the hold with the price, currency and expiry returned by step 2, then the event.
INSERT INTO holds (id, seat_id, user_id, status, expires_at, amount_cents, currency)
VALUES ($hold, $seat, $user, 'active', $expires_at, $price_cents, $currency);
INSERT INTO outbox (topic, payload)
VALUES ('seat.changed', jsonb_build_object('event_id', $event, 'seat_id', $seat));

COMMIT;
```

When two requests race, the second waits on the seat row lock. After the first commits, it re-evaluates the WHERE clause, no longer matches, and returns zero rows. That commit is the linearization point.

**Recovery on zero rows.** Roll back, then read once. If the caller has a hold on this seat that is `active` with `expires_at > clock_timestamp()`, or is `payment_pending`, return it with 200, so duplicate concurrent requests from one customer are idempotent. Otherwise check that the seat exists within the event: return 404 if it does not, and 409 `seat_unavailable` if it does.

### Release a hold

```sql
BEGIN;
UPDATE holds SET status = 'released', updated_at = clock_timestamp()
 WHERE id = $hold AND user_id = $user AND status = 'active'
RETURNING seat_id;           -- zero rows: 404 if not the owner, 409 if not active

UPDATE seats SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
 WHERE id = $seat AND current_hold_id = $hold;
INSERT INTO outbox (topic, payload) VALUES ('seat.changed', ...);
COMMIT;
```

### Start a payment

Run the guarded transition first, and look up the idempotency key only when it fails. Looking the key up first would race: two requests with the same key could both miss it, and the loser would see a generic hold error instead of the winner's attempt.

```sql
BEGIN;
UPDATE holds SET status = 'payment_pending', updated_at = clock_timestamp()
 WHERE id = $hold AND user_id = $user AND status = 'active'
   AND expires_at > clock_timestamp()
RETURNING seat_id, amount_cents, currency;   -- zero rows: ROLLBACK, then re-read (below)

UPDATE seats SET hold_expires_at = NULL      -- the seat is now protected from expiry
 WHERE id = $seat AND current_hold_id = $hold;

INSERT INTO payment_attempts (id, hold_id, user_id, idempotency_key, amount_cents, currency,
                              deadline_at, traceparent)
VALUES ($attempt, $hold, $user, $key, $amount_cents, $currency,
        clock_timestamp() + $payment_deadline, $traceparent);  -- unique violation: ROLLBACK, re-read
COMMIT;
```

The price comes from the hold's snapshot, never from the current seat row.

**Re-read after a loss.** If the hold UPDATE returns zero rows, or the insert violates a unique constraint, roll back and read once:

1. Find the caller's attempt by `(user_id, idempotency_key)`. If it belongs to this hold, return it. If it belongs to another hold, return 409 `idempotency_key_reused`.
2. Otherwise, if this hold already has an attempt, return 409 `payment_in_progress` while it is open and 409 `hold_not_active` once it is final.
3. Otherwise return 404 for a missing or foreign hold, or 409 `hold_not_active`.

The loser's UPDATE waits on the winner's row lock, so the winner has already committed when the loser reads.

| Situation | Result |
| --- | --- |
| Missing or malformed `Idempotency-Key` header | 400 `idempotency_key_required` |
| Same user and key, same hold, attempt still open | 202 with the existing attempt |
| Same user and key, same hold, attempt final (succeeded or failed) | 200 with the existing attempt |
| Same user, different key, hold already has an open attempt | 409 `payment_in_progress` |
| Same user, different key, the hold's attempt is already final | 409 `hold_not_active` |
| Same user and key already used on another hold | 409 `idempotency_key_reused` |
| Hold missing or owned by someone else | 404 |
| Hold expired, released or otherwise not payable, and no matching attempt | 409 `hold_not_active` |

**Expiry semantics.** The `expires_at` guard is evaluated when the UPDATE runs, not at commit. A payment that passes the guard while the hold is valid stays valid even if the commit lands a few milliseconds after `expires_at`.

The request returns as soon as this transaction commits. The worker does the rest, described under Background workers.

## Background workers

The `worker` binary runs four loops. Correctness never depends on the sweeper: acquire already reclaims expired seats lazily. The loops make the system tidy, visible and self-healing.

### Payment settlement

1. **Claim.** Select due attempts with `FOR UPDATE SKIP LOCKED`, push `next_check_at` forward by a 30 s lease, increment `attempts`, and commit. The lease is an optimization, not an exclusive lock: if the provider call outlasts it, another worker may claim the attempt. That is safe because the provider idempotency key is stable and settlement is guarded. Set the provider request timeout (for example 10 s) well below the lease.
2. **Call the provider outside any transaction.** An attempt in `created` calls `CreateCharge` using the attempt id as the provider idempotency key. An attempt in `pending` calls `GetCharge` for the same key. If `GetCharge` answers `unknown`, the worker calls `CreateCharge` again; that is safe whether or not the earlier request arrived, because `CreateCharge` is idempotent on the key. Checks repeat until the provider gives a terminal answer. Backoff doubles from 2 s with jitter up to a 1 minute cap, and after `deadline_at` the re-check interval is fixed at 10 minutes, so a stuck attempt is never starved.
3. **Apply the result in one transaction**, guarded by the attempt's current status so retries are no-ops. Only `created` and `pending` accept a result. `succeeded` and `failed` are final.

| Provider result | Attempt | Hold | Seat | Also |
| --- | --- | --- | --- | --- |
| Succeeded, attempt `created` or `pending` | `succeeded` | `confirmed` | `sold` | Insert the `orders` row with INSERT ... SELECT from the attempt and hold, never from application parameters; outbox `seat.changed` and `order.confirmed` |
| Declined, attempt `created` or `pending` | `failed` | `payment_failed` | `available` | Outbox `seat.changed` |
| Processing, unknown, timeout, 5xx or network error | `pending` | unchanged | unchanged | Store `last_error`; next check after exponential backoff with jitter |
| Still `created` or `pending` at `deadline_at` | unchanged | unchanged | unchanged | The attempt counts as overdue: raise the `payments_overdue` gauge and alert; keep checking; the seat stays held |

**Attempt status transitions.** `succeeded` and `failed` are final. A decline is terminal and the provider never reverses it, which is why a late success after `failed` cannot happen with the mock. A real provider might not keep that promise, and a hand-closed attempt (see Stuck payments) is not a provider decline; the Extensions section describes the extra states it would need.

| Current status | Event | Next status |
| --- | --- | --- |
| `created` | Success | `succeeded` |
| `created` | Decline | `failed` |
| `created` or `pending` | Timeout, 5xx or still processing | `pending` |
| `pending` | Success | `succeeded` |
| `pending` | Decline | `failed` |
| `succeeded` | Any later event | `succeeded` (ignored) |
| `failed` | Any later event | `failed` (ignored) |

**Provider responses.** The `Provider` port has two calls, `CreateCharge` and `GetCharge`, both idempotent on the key. Only a decline may release a seat.

| Provider response | Meaning | May release the seat? |
| --- | --- | --- |
| `succeeded` | The charge completed | No; confirm the order |
| `declined` | A definitive, terminal rejection | Yes |
| `processing` | The charge is still running | No; keep checking |
| `unknown` | The outcome cannot be established: a timeout, a 5xx, or a key the provider has never seen | No; keep checking, and re-issue `CreateCharge` (idempotent on the key) |

**No webhooks in v1.** The worker learns outcomes only by calling `CreateCharge` and `GetCharge`, retrying with backoff until the provider answers. Webhooks and their security checks are described under Extensions.

**Stuck payments.** A payment still `created` or `pending` past `deadline_at` keeps its seat held and raises the `payments_overdue` alert. The settlement loop recomputes `payments_overdue` on every tick as the count of such attempts, independent of whether any attempts were claimed, so it needs no reset and falls as they settle. v1 has no operator tooling, so a person follows this runbook. Cancelling at the provider and refund tracking are described under Extensions.

1. Look up the key at the provider (for the mock, `mockpay.charges`; for a real provider, its dashboard). If the provider reports a successful charge, do not release the seat: settle the attempt as succeeded and stop. If the charge is still `processing`, leave the seat held.
2. Only if the provider has no successful charge and will accept none, release the seat by hand in one transaction, locking the attempt, then the hold, then the seat: set the attempt to `failed` with `last_error = 'manual_release'`, the hold to `payment_failed`, and the seat to `available` with `current_hold_id` and `hold_expires_at` cleared, then write a `seat.changed` outbox row. With a real provider, "no charge yet" does not prove that none will appear, which is why the cancel protocol is listed under Extensions.
3. Never mark an attempt `failed` while a successful charge exists, and never release a seat because of a timeout alone. `failed` normally means the provider declined; `last_error` records that this row was closed by hand.

Keep this runbook in `docs/runbooks/manual-payment-resolution.md`.

### Expiry sweeper

Every 5 s, in batches, select `active` holds past `expires_at` in id order with `FOR UPDATE SKIP LOCKED`, mark them `expired`, and in the same transaction set their seats back to `available` where `current_hold_id` still matches, also in id order. Write one `seat.changed` outbox row per seat. It never touches `payment_pending` or `confirmed` holds, and several worker instances can run it safely.

### Outbox relay

Every second, claim a batch of unpublished outbox rows with `FOR UPDATE SKIP LOCKED`, set `claimed_until` to a 30 s lease, and commit. Run the handlers outside any transaction, then set `published_at` on the rows that succeeded. Failed or abandoned claims become claimable again when the lease expires, so no row lock is ever held across an external call. Delivery is at least once, so handlers must be idempotent. The first handler deletes the Redis seat-map key. A queue such as SQS can replace the relay later without changing the producers.

### Invariant checker

Every minute, and after each integration test, run queries that must return zero rows. Count any result in `invariant_violations_total` and log the offending ids. The checker only detects and reports; it never repairs data, so treat any violation as a bug.

```sql
-- seat not available, but its hold is missing
SELECT s.id FROM seats s LEFT JOIN holds h ON h.id = s.current_hold_id
 WHERE s.status <> 'available' AND h.id IS NULL;
```

- A `sold` seat without an `orders` row, or an order whose seat is not `sold`.
- A `held` seat whose hold is neither `active` nor `payment_pending`.
- A live hold that its seat does not point back to.
- A `succeeded` attempt without an order.
- An order whose `user_id`, `amount_cents` or `currency` differs from its payment attempt.
- A seat whose `hold_expires_at` disagrees with its hold: it must equal `holds.expires_at` for an `active` hold and be NULL for `payment_pending` and `confirmed` holds.

## Redis usage

Redis speeds up reads and limits abuse; it never decides who owns a seat. Add it in Phase 4, after the reservation and payment paths already pass their tests without it. In code, the hold and payment use cases take no cache port, so they cannot read Redis by construction.

- **Seat map.** Key `seatmap:{event_id}` holds a compact JSON array of seat id, section, row, number, status and price. TTL is 5 s. A cache miss reads PostgreSQL and populates the key, with `singleflight` so one miss triggers one query.
- **Invalidation.** The outbox relay deletes the key on every `seat.changed` event. Readers may still see a stale `available` seat for up to the TTL or relay lag. That is acceptable because the hold endpoint checks PostgreSQL and returns 409.
- **Rate limiting.** A per-user limit on `POST /events/{id}/holds`, using a fixed window or token bucket in Redis. The limit is configurable.
- **Failure behavior.** If Redis errors, the seat map falls back to PostgreSQL and the rate limiter fails open. Both increment `redis_errors_total`.
- **Never in Redis.** Locks, hold state, payment state, or anything the invariants depend on.

Optional experiment after the benchmark: if hundreds of clients fight over one hot seat and the row lock is the bottleneck, add a Redis pre-check that rejects obvious losers before they reach PostgreSQL. Keep the PostgreSQL update as the only arbiter, and publish the before and after numbers.

## Testing and acceptance

The project is done when every invariant has a passing test and every injected failure ends in a clean invariant check. Run everything with `go test -race ./...`; CI runs the integration tests too.

**Layers**

- **Unit:** transition functions, backoff and jitter, request validation, the mock provider, and use cases against in-memory fakes of the ports.
- **Integration:** Testcontainers for PostgreSQL and Redis, real HTTP through `httptest`, the worker running in-process. The use-case suite also runs against the PostgreSQL adapter as a contract test; concurrency tests run only there, because a fake cannot reproduce row-lock behavior. Run the invariant checker at the end of every test.
- **Concurrency:**
  - C1: 1,000 goroutines request the same seat. Expect exactly one success and 999 seat\_unavailable results. Run C1 to C3 directly against the use cases and PostgreSQL, with no HTTP layer, and write them before the handlers.
  - C2: 10,000 requests across 100 seats. Expect exactly 100 holds and no seat with two live holds.
  - C3: a randomized mix of hold, release, pay and expire with the worker running. The checker must report zero violations.
- **Fault injection:** the table below.

**Fault injection**

| # | Fault | Expected outcome |
| --- | --- | --- |
| 1 | Kill the worker after the payment starts, before the provider call | After the lease lapses another claim calls the provider; one charge |
| 2 | Provider charges, then the response times out (`timeout_after_charge`) | Attempt stays `pending`; the next check calls `GetCharge`, finds the charge and settles it; the seat is never released and ends sold |
| 3 | Provider charges, then the worker crashes before settling | The attempt is still `created`, so recovery calls `CreateCharge` again; the idempotent mock returns the existing charge from `mockpay.charges` and settlement applies it; exactly one order and one charge |
| 4 | Provider declines | Hold `payment_failed`, seat `available`, another customer can acquire it |
| 5 | A PostgreSQL connection is cut mid-transaction | Rollback, a 5xx to the client, no partial state, checker clean |
| 6 | Redis is stopped (from Phase 4) | Holds and payments work; the seat map comes from PostgreSQL |
| 7 | A pay request waits on the hold row lock, held open by the test, until after the hold expires | The guard sees the expired hold: 409 and the seat is reusable. A payment whose guarded UPDATE ran before expiry stays valid even if its commit lands later |
| 8 | The provider never gives a final answer before `deadline_at` (mock behavior `unresolved`) | Attempt stays `pending`, the seat stays held, the worker keeps re-checking, and the `payments_overdue` gauge is raised |
| 9 | Two simultaneous hold requests from the same user for the same seat | One 201 and one 200 with the same hold id; no 409 |
| 10 | A second payment is attempted with a different key after the provider declined | 409 `hold_not_active`; no second attempt row exists |
| 11 | A guard succeeds but its dependent UPDATE affects zero rows (injected) | Rollback, an internal error, `consistency_errors_total` incremented, no partial state |

**Mock provider.** An adapter behind the `Provider` port that runs inside the worker and the tests. Its state lives in its own PostgreSQL schema (`mockpay`), one row per idempotency key, so charge records survive killing the worker. It offers `CreateCharge` and `GetCharge`, both idempotent on the key, with four behaviors chosen by `MOCK_BEHAVIOR` or by the test: `success`, `decline`, `timeout_after_charge` (record a succeeded charge, then return a timeout) and `unresolved` (record a `processing` charge and return `processing`, so `GetCharge` also answers `processing`). A key the provider has never seen always answers `unknown`. It also counts real charges per key for assertions. Two limits to keep in mind. Because there is no network boundary, the adapter uses its own connection pool and is never handed the use case's transaction, and a test fails if it is called while an application transaction is open; that keeps "never call the provider inside a transaction" enforceable. And it shares PostgreSQL's availability, unlike a separate server. It is deliberately simple; a real provider needs more, as described under Extensions.

## Phases and exit criteria

Build in six phases and move on only when a phase's exit criteria pass. Phases 1 to 3 make a complete project; 4 to 6 are extensions.

1. **Foundation.** Repo, module layout with the domain, app and adapters packages, an import-direction test, docker-compose with PostgreSQL, migrations, pgx pool, config, `slog`, `/healthz` and `/readyz`, event and seat read endpoints, CI with lint and unit tests.
   - Exit: `docker compose up` serves the read endpoints and CI is green.
2. **Seat reservation.** Acquire, release and expiry sweeper, outbox writes, invariant checker, use cases behind ports, and ADRs on PostgreSQL versus Redis and on where invariants live.
   - Exit: C1 and C2 pass with `-race`; invariants 1, 2, 3 and 5 have tests; the domain-versus-SQL agreement test passes.
3. **Payments.** Build it in the four stages below: the start and settle payment use cases (settlement runs through the transaction port), the payment endpoint with status polling, the settlement worker, the in-process mock provider with its own `mockpay` schema, and an ADR on the payment state machine.
   - Exit: every fault-injection case except #6 (Redis) passes; invariants 4, 6, 7, 8 and 9 have tests; C3 reports zero violations.
4. **Redis and observability.** Seat-map cache, outbox relay handler, rate limiting, Prometheus metrics, OpenTelemetry traces across API, worker and outbox.
   - Exit: the Redis-stopped test passes (invariant 10) and the seat map is served from cache under load.
5. **Benchmark.** k6 or oha scripts, a baseline run, and the results written to `docs/benchmarks/`.
   - Exit: a stranger can reproduce the numbers from the README.
6. **AWS.** Infrastructure as code, CI deployment, teardown.
   - Exit: deploy, smoke test and destroy all work from a clean account state.

**Phase 3 stages.** Each stage ends in a working, tested system, so you can stop after any of them.

1. **Always succeeds.** `StartPayment` creates the attempt, the worker calls a mock that always succeeds, and settlement confirms the hold, sells the seat and creates the order. Exit: the full flow works end to end, invariants 4, 6 and 7 have tests, and fault cases 7 and 11 pass for the payment transitions.
2. **Declines.** Add the decline behavior: the hold becomes `payment_failed` and the seat is available again. Exit: fault cases 4 and 10 pass.
3. **Timeout after charge and unresolved.** Add the `timeout_after_charge` and `unresolved` behaviors, `GetCharge`, polling with capped backoff and the overdue alert. Exit: fault cases 2 and 8 pass, and invariant 8 has a test.
4. **Crash recovery.** Kill the worker at each crash point. Exit: fault cases 1 and 3 pass, and C3 reports zero violations.

Fault cases 5 and 9 are Phase 2 work (they cover the database connection and acquire) and must keep passing here. Case 6 waits for Phase 4.

**Stop rule.** After Phase 3, write the README and start applying with it. Continue with Phases 4 to 6 in parallel with the job search, not before it.

## Observability, configuration and benchmarks

Instrument from Phase 1 so every later phase can be debugged from logs and metrics instead of guesswork.

**Logging and tracing**

- Structured JSON logs with `log/slog`, carrying `request_id`, `hold_id`, `payment_id` and `seat_id` where known.
- OpenTelemetry spans for HTTP, pgx and provider calls. Store `traceparent` on outbox rows and payment attempts so a trace continues into the worker.

**Prometheus metrics**

- `hold_attempts_total{result}` with results `acquired`, `conflict`, `error`.
- `hold_acquire_duration_seconds` histogram.
- `payments_total{result}` and the `payments_overdue` gauge (alert on any non-zero value).
- `outbox_lag_seconds`, `invariant_violations_total` and `consistency_errors_total` (alert on any non-zero violation or error count).
- `seatmap_cache_total{result}` and `redis_errors_total`.
- `pgxpool_acquired_conns` and pool wait time.

**Configuration (environment variables)**

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | none | PostgreSQL connection string |
| `REDIS_URL` | empty | Redis; empty disables caching and rate limiting |
| `HOLD_TTL` | 10m | Lifetime of an active hold |
| `PAYMENT_DEADLINE` | 15m | Time after which a still-pending payment counts as overdue and raises an alert |
| `PAYMENT_LEASE` | 30s | How long a claimed payment attempt is left alone by other workers |
| `PROVIDER_TIMEOUT` | 10s | Request timeout for provider calls; keep it well below the lease |
| `SWEEP_INTERVAL` | 5s | Expiry sweeper period |
| `PG_MAX_CONNS` | 25 | pgx pool size |
| `MOCK_BEHAVIOR` | success | `success`, `decline`, `timeout_after_charge` or `unresolved`; tests override it per call |

**Benchmark protocol**

- Set no pass or fail target. Measure a baseline first, then change one thing at a time.
- Record the hardware or instance sizes, client concurrency, seat distribution (one hot seat, uniform, skewed), throughput, p50, p95 and p99 latency, error rate, pool utilization and PostgreSQL CPU.
- Run the load generator in the same network as the API. Save each run with the commit hash and the exact command in `docs/benchmarks/`.
- Explain the bottleneck you found, with a profile or metrics, rather than quoting a headline number.

## AWS deployment

Deploy to AWS only after Phases 1 to 5 work locally, and build it so one command creates the stack and one command destroys it. Pick one infrastructure tool and know it well: Terraform is the default because it is already on the resume, and Pulumi in Go is an acceptable swap.

**Target shape**

- ECS Fargate running two services from one image with different entrypoints: `api` behind an Application Load Balancer, and `worker` with no public ingress. The mock provider runs inside the worker.
- RDS PostgreSQL, single-AZ, in private subnets; the mock provider uses its own schema. ElastiCache Redis, single node.
- ECR for images, Secrets Manager or SSM Parameter Store for credentials, CloudWatch for logs.
- Run migrations as a one-off ECS task before each deploy.

**Delivery pipeline**

1. GitHub Actions runs lint, unit and integration tests; a red build stops here.
2. Build the image from a multi-stage Dockerfile on the current Go 1.27 patch, with a small non-root runtime image.
3. Push to ECR, run the migration task, then apply the infrastructure change and roll the services.
4. Run a smoke test: create a hold and read it back through the load balancer.

**Cost notes**

- The load balancer, RDS, ElastiCache and Fargate tasks bill by the hour whether or not anyone calls them. Check current pricing before provisioning.
- A NAT gateway is the usual surprise. Avoid it if the design allows.
- Destroy the stack between demos, and confirm that `destroy` followed by `up` works from scratch.
- Skip autoscaling in v1. Run a fixed small task count and document the choice.

## Risks, open questions and cuts

The main risk is scope: a half-built version of everything is worth less than a finished reservation and payment core.

**Open questions to settle before Phase 1**

- [ ] HTTP router: standard library (default), Chi or Gin?
- [ ] Infrastructure tool: Terraform (default) or Pulumi in Go?
- [ ] Query layer: hand-written pgx SQL (default for the critical paths), with or without `sqlc` for plain CRUD?
- [ ] Identity: keep the `X-User-ID` stub, or add JWT in Phase 1?
- [ ] Add a real payment provider in test mode after the mock, or stop at the mock?
- [ ] Use a hosted demo, or ship only the repository and recorded benchmark results?

**Known risks**

- **Hot-seat contention.** Many clients on one seat queue on one row lock, and the pgx pool can saturate. Measure it in Phase 5 before adding any mitigation.
- **Stale seat maps.** Cache and outbox lag can show a held seat as available. The 409 on hold is the safety net.
- **Overdue payments.** A payment that never resolves keeps its seat held, by design. v1 only alerts, so watch `payments_overdue` and expect to fix such rows by hand, following the runbook under Background workers.
- **Multi-seat carts.** Not in v1. When added, lock seats in sorted id order to avoid deadlocks, and hold or fail the whole set.
- **Single database primary.** Using the database clock is safe only because one primary decides every expiry. Note this in the ADR.
- **Cloud cost.** Mitigated by building and testing locally first and destroying the stack between demos.
- **Layering overhead.** The ports-and-adapters split can eat the schedule. Judge Phase 2 on concurrency correctness, not architecture purity. If the layers slow you down, fold `app` into the PostgreSQL adapter and keep `domain` pure.

**Explicitly cut from v1**

- Virtual waiting room, multi-region, Kubernetes, microservices, autoscaling.
- Redis-based seat locks, and any Redis-to-PostgreSQL reconciliation job.
- Chaos tooling beyond the targeted fault-injection tests.
- Mappers between every layer, and one interface per struct.
- Webhooks, operator tooling, provider cancellation and refund tracking; see Extensions.

## Extensions

These designs were worked out and then cut, because they add many states and fault cases for failures that the mock provider cannot cause. Add one only when you integrate a provider that can fail that way, and bring its fault cases with it.

| Extension | What it adds | Add it when |
| --- | --- | --- |
| Webhooks | The provider pushes outcomes. The handler verifies a signed, time-stamped request with a constant-time comparison and a body size cap, then in one transaction records the provider event id (`ON CONFLICT DO NOTHING`), settles the attempt, creates the order and marks the event processed. An event for an unknown attempt rolls back and returns a retryable 5xx. | Polling latency matters, or the provider offers webhooks |
| `needs_review` status | After `deadline_at`, mark the attempt `needs_review` and keep re-checking it slowly; a late provider result can still settle it. | You want the overdue state in the data, not only in a gauge |
| Provider cancel and operator resolve | `CancelCharge`, serialized with `CreateCharge` on the key by a row lock in the provider's database. `POST /admin/payments/{id}/resolve` releases a seat only after the provider answers `cancelled` or `declined`; `not_found` and `unknown` never release one. | Operators need to unstick payments |
| Late-success compensation | A `refund_required` status for a success that arrives after `failed`, then `refunded`, set by an idempotent `POST /admin/payments/{id}/refunded` with an `X-Operator` header and a CHECK constraint on the refund fields. | A provider cannot promise that a decline is final |
| Separate mock server | The mock as its own process with its own database role, so it behaves like a real network boundary. | You want a more realistic demo |
| Real provider | An adapter for a service such as Stripe. Treat cancel as best effort and fall back to `refund_required` when a charge appears late. | You want to accept real payments |

Fault cases to bring with them: a delayed success after the attempt was written off, a cancel racing a create, a duplicate and an out-of-order webhook, and a lost refund response.
