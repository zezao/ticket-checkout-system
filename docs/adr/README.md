# Architecture Decision Records

Short records for decisions that shape the system. Each record states the
context, the decision, and its consequences.

Naming: `NNNN-short-title.md`, numbered sequentially.

Written:

- [0001: PostgreSQL owns seat state](0001-postgresql-owns-seat-state.md)
- [0002: Where invariants live](0002-where-invariants-live.md)

Planned:

- `0003-payment-state-machine.md` (Phase 3): persisted attempts, idempotency
  keys, and why a timeout means unknown rather than failed.
