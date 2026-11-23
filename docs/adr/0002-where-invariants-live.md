# 0002: Where Invariants Live

Status: Accepted (Phase 2)

## Context

Hold state has legal transitions (for example `active -> released`, and nothing
leaving `confirmed`). An in-memory object can specify those rules, but it cannot
stop two concurrent requests from both observing an available seat. The database
can.

## Decision

Each rule has two forms:

- A pure function in `internal/domain/reservation` decides which transitions are
  legal and returns typed errors. Domain code imports only the standard library.
- A SQL guard in `internal/adapters/postgres` makes the important transitions
  atomic. `TryTransition` applies the same status guard used by the write paths;
  `TestHoldTransitionAgreement` runs all status pairs through both and requires
  the same accept or reject result.

Other rules that follow from this split:

- **Atomic ports.** `HoldStore.Acquire`, `Release`, and `ExpireBatch` document
  atomic operations. Only the PostgreSQL adapter can honor that contract.
- **One transaction.** Acquire, release, and expiry write the hold, the seat,
  and the outbox row together. Settlement in Phase 3 will use the same pattern
  through the transaction port.
- **Lock order.** Locks follow one global order: payment attempt, then hold, then
  seat. Acquire retires an expired hold before claiming the seat. Release and the
  sweeper lock the hold before the seat.
- **Row-count rule.** After a guard succeeds, every dependent statement must
  affect exactly the expected rows. A mismatch rolls back and returns an
  internal error. The observable symptom is a 5xx and a clean retry, never a
  half-applied state.
- **Invariant checker.** Independent SQL queries report data-level consequences
  of invariants 1, 2, 5, and 7 (and the payment checks that Phase 3 will use).
  The checker only reports; it never repairs.

## Consequences

- The domain and SQL can drift; the agreement test is the guard against that.
  Every new transition adds a pair to the table.
- Critical paths use hand-written SQL rather than a generated mapper. That is
  intentional: the exact guard and row counts matter.
- The rules are visible in two places instead of hidden in one. The ADR and the
  agreement test are what keep them honest.
- Metrics for `consistency_errors_total` arrive with the Phase 4 observability
  work; until then consistency errors are logged as internal errors.
