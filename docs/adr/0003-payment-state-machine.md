# 0003: Payment State Machine

Status: Accepted (Phase 3)

## Context

A charge request can time out after the provider has already charged the card.
A timeout therefore means "unknown", not "failed". Releasing the seat on a
timeout can sell it twice, while retrying a charge can double-charge unless the
provider asks are idempotent. At the same time, a successful charge must end in
exactly one order.

## Decision

- **A persisted attempt.** `payment_attempts` is the state machine. Each row has
  a status (`created`, `pending`, `succeeded`, `failed`), an idempotency key, a
  provider deadline and a `next_check_at` schedule.
- **One attempt per hold.** `UNIQUE (hold_id)` means a declined hold cannot be
  retried. The customer takes a new hold instead, so every hold has one
  unambiguous payment history.
- **Keys are scoped per user.** `UNIQUE (user_id, idempotency_key)` prevents one
  customer's key from colliding with or revealing another's. The provider
  idempotency key is the attempt id.
- **Only a decline releases.** `succeeded` confirms the hold and sells the seat;
  `declined` fails the hold and returns the seat; `processing`, `unknown`,
  timeouts and 5xx responses leave the attempt `pending` and the seat held.
- **Settlement is one transaction.** Guarded by the attempt status, it updates
  the attempt, the hold and the seat, inserts the order with `INSERT ... SELECT`
  from the attempt and hold, and writes `seat.changed`/`order.confirmed` to the
  outbox. Locks follow attempt, then hold, then seat.
- **Provider calls happen outside transactions.** The worker claims due attempts
  with `FOR UPDATE SKIP LOCKED` and a lease, commits, calls the provider under
  `PROVIDER_TIMEOUT`, then applies the result in a new transaction. The lease is
  an optimization; the stable provider key and the status guard keep retries
  safe.
- **Backoff.** Re-checks double from 2 s with jitter up to 1 minute. After
  `deadline_at`, the interval is fixed at 10 minutes and the attempt counts as
  overdue; v1 alerts and keeps the seat held.
- **One transaction across contexts.** Reservation, Payments and Ordering share
  one database transaction for settlement instead of events and eventual
  consistency. Splitting the contexts would add intermediate states and failure
  modes that v1 does not need.

## Consequences

- A payment that never resolves keeps its seat held until a person acts. The
  runbook is `docs/runbooks/manual-payment-resolution.md`; the v1 system only
  raises the overdue alert.
- The mock provider persists charges in its own `mockpay` schema, so a worker
  restart cannot lose idempotency. A real provider needs the same guarantee.
- A definitive decline is final in the mock. A provider that can reverse a
  decline would need late-success compensation and refund tracking, described
  under Extensions in the spec.
- `TryTransition` and the hold agreement test keep the domain and SQL views of
  the hold lifecycle aligned; the payment transition table is covered by unit
  tests.
