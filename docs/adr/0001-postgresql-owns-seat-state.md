# 0001: PostgreSQL Owns Seat State

Status: Accepted (Phase 2)

## Context

A seat can have many simultaneous buyers. The system must pick one without
losing a write on failover or allowing two live holds for the same seat.

Redis replication is asynchronous. A lock acknowledged by a Redis primary can
disappear when a replica is promoted, and two clients can then believe they own
the same seat. Application-level mutexes and in-memory sets have the same problem
across processes and restarts.

## Decision

PostgreSQL arbitrates seat ownership:

- `POST /events/{id}/holds` runs one transaction with a conditional `UPDATE` on
  the seat row, then inserts the hold and its outbox event.
- `holds_one_live_per_seat`, a partial unique index over
  `(seat_id) WHERE status IN ('active', 'payment_pending', 'confirmed')`, is the
  backstop against two live holds, even if a future code path forgets a guard.
- Expiry and payment transitions are guarded by status and by
  `expires_at`/`clock_timestamp()` in the same `UPDATE`.
- Every expiry uses the database clock. `now()` is frozen at transaction start
  and application clocks drift between instances.

Redis, when it arrives in Phase 4, is only a seat-map cache and a rate limiter.
The hold and payment use cases take no cache port, so they cannot read Redis.

## Consequences

- Requests for one hot seat serialize on one row lock. That is acceptable and
  measurable; Phase 5 benchmarks it before any mitigation is added.
- Correctness assumes a single primary that decides every expiry. This is noted
  as a known limit of the design.
- A stale cached seat map can show a held seat as available. The hold endpoint
  re-checks PostgreSQL and returns 409, so the cache stays harmless.
- If Redis disappears, reads fall back to PostgreSQL and the rate limiter fails
  open; no invariant depends on Redis.
