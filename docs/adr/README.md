# Architecture Decision Records

Short records for decisions that shape the system. Each record states the
context, the decision, and its consequences. The spec in
`docs/spec/ticket-checkout-system.md` lists the decisions; the ADRs are written
alongside the phase that implements them.

Naming: `NNNN-short-title.md`, numbered sequentially.

Planned:

- `0001-postgresql-owns-seat-state.md` (Phase 2): why PostgreSQL, not Redis,
  arbitrates seat ownership.
- `0002-where-invariants-live.md` (Phase 2): domain transition functions specify
  the rules; SQL guards enforce them atomically.
- `0003-payment-state-machine.md` (Phase 3): persisted attempts, idempotency
  keys, and why a timeout means unknown rather than failed.
