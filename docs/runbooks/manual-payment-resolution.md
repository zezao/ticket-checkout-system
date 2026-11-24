# Manual Payment Resolution

Use this runbook when a payment attempt stays `created` or `pending` past
`deadline_at`. The worker raises `payments_overdue` (currently logged as
`payments overdue`) and keeps re-checking; the seat stays held by design. v1 has
no operator tooling, so a person resolves the row.

## 1. Find the attempt

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c "
SELECT id, hold_id, user_id, status, attempts, deadline_at, last_error
  FROM payment_attempts
 WHERE status IN ('created', 'pending')
   AND deadline_at < clock_timestamp()
 ORDER BY deadline_at;"
```

The provider idempotency key is the attempt id.

## 2. Ask the provider for the truth

For the mock:

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c "
SELECT idempotency_key, status, amount_cents, currency
  FROM mockpay.charges
 WHERE idempotency_key = '<attempt-id>';"
```

For a real provider, look up the key in its dashboard.

- If the provider reports a successful charge, **do not release the seat**.
  Settle the attempt as succeeded and stop. The worker will normally do this on
  the next check; only intervene if it cannot.
- If the charge is still `processing`, leave the seat held and keep checking.
- Only if there is no successful charge **and none can appear** may you release
  the seat by hand.

## 3. Release by hand (last resort)

One transaction, locking the attempt, then the hold, then the seat. Never mark an
attempt failed while a successful charge exists. Never release a seat because of
a timeout alone.

```sql
BEGIN;

UPDATE payment_attempts
   SET status = 'failed',
       last_error = 'manual_release',
       updated_at = clock_timestamp()
 WHERE id = '<attempt-id>'
   AND status IN ('created', 'pending')
RETURNING id;

UPDATE holds
   SET status = 'payment_failed', updated_at = clock_timestamp()
 WHERE id = '<hold-id>'
   AND status = 'payment_pending'
RETURNING id;

UPDATE seats
   SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
 WHERE current_hold_id = '<hold-id>'
   AND status = 'held'
RETURNING id, event_id;

-- Write one seat.changed row for the released seat.
INSERT INTO outbox (topic, payload)
SELECT 'seat.changed',
       jsonb_build_object('event_id', s.event_id, 'seat_id', s.id)
  FROM seats s
 WHERE s.id = '<seat-id>';

COMMIT;
```

Verify with the invariant queries and run the worker's checker; it must stay
silent:

```sh
docker compose restart worker
docker compose logs -f worker
```

With a real provider, "no charge yet" does not prove that none will appear.
The cancel/refund protocol is under Extensions in the specification.
