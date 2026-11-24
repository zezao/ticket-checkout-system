# Testing Playbook

How to verify the system from the outside, from fastest to most hands-on. The
commands assume the repository root and a running Docker daemon.

## Automated suites

```sh
make test-unit   # unit tests + fuzz seeds; no Docker
make test        # go test -race ./...; starts a PostgreSQL 17 container
make lint        # golangci-lint + depguard layer rules
```

| Command | What it covers |
| --- | --- |
| `make test-unit` | Config, handlers, use cases, domain transitions, sweeper/checker/relay loops. No Docker needed. |
| `make test` | Everything above plus real HTTP + PostgreSQL + Redis: holds, payments, all 36 transition pairs, C1/C2/C3, faults 1-5 and 7-11, cache/rate-limit tests, invariant 10. |
| `make lint` | `depguard` proves `domain` imports only stdlib and adapters do not reach into `cmd`. |

Expected: every package `ok`, no race reports, and `0 issues` from lint. The full
integration suite takes roughly 7 seconds locally.

## Manual end-to-end against Compose

Start from a clean database (this deletes all local data):

```sh
docker compose down -v
docker compose up -d --build
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/readyz)" = 200 ]; do sleep 1; done
docker compose ps -a
```

Expected: `postgres` healthy, `migrate` and `seed` `Exited (0)`, `api` and
`worker` `Up`.

```sh
BASE=http://localhost:8080
EVENT_ID=0199a000-0000-7000-8000-000000000001
SEAT_ID=0199a000-0000-7000-8000-000000000202
USER_A=0199a000-0000-7000-8000-000000000911
USER_B=0199a000-0000-7000-8000-000000000912

curl -sS $BASE/healthz; echo
curl -sS $BASE/readyz; echo
curl -sS $BASE/events | python3 -m json.tool
curl -sS $BASE/events/$EVENT_ID/seats | python3 -m json.tool
```

Paste this whole block to exercise the hold lifecycle. It prints PASS/FAIL per
expected result:

```bash
BASE=http://localhost:8080
EVENT_ID=0199a000-0000-7000-8000-000000000001
SEAT_ID=0199a000-0000-7000-8000-000000000202
USER_A=0199a000-0000-7000-8000-000000000911
USER_B=0199a000-0000-7000-8000-000000000912

req() {
  if [ -n "$4" ]; then
    curl -sS -o /tmp/resp.json -w '%{http_code}' -X "$1" "$BASE$2" \
      -H "X-User-ID: $3" -H 'Content-Type: application/json' -d "$4"
  else
    curl -sS -o /tmp/resp.json -w '%{http_code}' -X "$1" "$BASE$2" \
      -H "X-User-ID: $3"
  fi
}
expect() {
  if [ "$1" = "$2" ]; then echo "PASS: $3"; else echo "FAIL: $3 (got $1, want $2)"; fi
}
body() { python3 -m json.tool /tmp/resp.json; }
holdid() { python3 -c 'import json; print(json.load(open("/tmp/resp.json"))["hold"]["id"])'; }
holdstatus() { python3 -c 'import json; print(json.load(open("/tmp/resp.json"))["hold"]["status"])'; }

code=$(req POST "/events/$EVENT_ID/holds" "$USER_A" "{\"seat_id\":\"$SEAT_ID\"}")
expect "$code" 201 "A takes the seat"
HOLD_ID=$(holdid)
body

code=$(req POST "/events/$EVENT_ID/holds" "$USER_A" "{\"seat_id\":\"$SEAT_ID\"}")
expect "$code" 200 "A repeats and gets the same hold"
[ "$(holdid)" = "$HOLD_ID" ] && echo "PASS: same hold id" || echo "FAIL: different hold id"

code=$(req POST "/events/$EVENT_ID/holds" "$USER_B" "{\"seat_id\":\"$SEAT_ID\"}")
expect "$code" 409 "B is rejected"
body

code=$(req GET "/holds/$HOLD_ID" "$USER_B")
expect "$code" 404 "B cannot read A's hold"

code=$(req DELETE "/holds/$HOLD_ID" "$USER_B")
expect "$code" 404 "B cannot release A's hold"

code=$(req GET "/holds/$HOLD_ID" "$USER_A")
expect "$code" 200 "A can read the hold"
echo "status: $(holdstatus)"

code=$(req DELETE "/holds/$HOLD_ID" "$USER_A")
expect "$code" 204 "A releases the hold"

code=$(req GET "/holds/$HOLD_ID" "$USER_A")
expect "$code" 200 "A can still read the released hold"
echo "status: $(holdstatus)"

code=$(req DELETE "/holds/$HOLD_ID" "$USER_A")
expect "$code" 409 "releasing twice is rejected"
body

code=$(req POST "/events/$EVENT_ID/holds" "$USER_B" "{\"seat_id\":\"$SEAT_ID\"}")
expect "$code" 201 "B can take the seat after release"
B_HOLD_ID=$(holdid)

code=$(req DELETE "/holds/$B_HOLD_ID" "$USER_B")
expect "$code" 204 "cleanup"
```

What to look at in the JSON: the `hold` object and `payment: null`, the
`X-Request-Id` response header, and the `seat_unavailable` / `hold_not_active`
error envelopes.

## Payment lifecycle

Use a fresh seat and user (the hold script left its seat released):

```sh
BASE=http://localhost:8080
EVENT_ID=0199a000-0000-7000-8000-000000000001
SEAT_ID=0199a000-0000-7000-8000-000000000205
USER_ID=0199a000-0000-7000-8000-000000000921

# 1. Hold the seat.
HOLD_ID=$(curl -sS -X POST "$BASE/events/$EVENT_ID/holds" \
  -H "X-User-ID: $USER_ID" -H 'Content-Type: application/json' \
  -d "{\"seat_id\":\"$SEAT_ID\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["hold"]["id"])')
echo "hold: $HOLD_ID"

# 2. Start payment. Expect 202 with payment.status created.
curl -sS -i -X POST "$BASE/holds/$HOLD_ID/payment" \
  -H "X-User-ID: $USER_ID" -H 'Idempotency-Key: demo-key-1'

# 3. Poll until confirmed. The worker settles within about a second.
for i in $(seq 1 20); do
  BODY=$(curl -sS -H "X-User-ID: $USER_ID" "$BASE/holds/$HOLD_ID")
  read -r H P <<< "$(python3 -c '
import json,sys
d = json.load(sys.stdin)
print(d["hold"]["status"], d["payment"]["status"] if d["payment"] else "-")
' <<< "$BODY")"
  echo "poll $i: hold=$H payment=$P"
  [ "$H" = confirmed ] && break
  sleep 0.5
done

# 4. The same key returns the same final attempt with 200.
curl -sS -i -X POST "$BASE/holds/$HOLD_ID/payment" \
  -H "X-User-ID: $USER_ID" -H 'Idempotency-Key: demo-key-1'

# 5. A different key after the final result returns 409 hold_not_active.
curl -sS -i -X POST "$BASE/holds/$HOLD_ID/payment" \
  -H "X-User-ID: $USER_ID" -H 'Idempotency-Key: demo-key-2'
```

Expected: 202, then `created` followed by `confirmed` / `succeeded`; the same key
returns 200 with the same attempt id; a different key returns 409
`hold_not_active`. The seat becomes `sold`, one order exists, and
`mockpay.charges` has exactly one row for the attempt.

## Try the provider behaviors

Compose forwards `MOCK_BEHAVIOR`, `PAYMENT_DEADLINE`, `PAYMENT_LEASE`,
`PROVIDER_TIMEOUT` and `SWEEP_INTERVAL`, so these need no file edits. The first
charge recorded for a key never changes; use a fresh seat or
`docker compose down -v` between demos.

Decline:

```sh
MOCK_BEHAVIOR=decline docker compose up -d worker
# Acquire a fresh seat and start payment.
# Expected: hold payment_failed, payment failed, seat available, no order.
MOCK_BEHAVIOR=success docker compose up -d worker
```

Timeout after charge:

```sh
MOCK_BEHAVIOR=timeout_after_charge docker compose up -d worker
# Acquire a fresh seat and start payment.
# The first settlement records a succeeded charge but returns a timeout.
# Poll: payment stays pending. After the 2s backoff the worker calls GetCharge,
# finds the charge, and the payment becomes succeeded. The seat ends sold.
MOCK_BEHAVIOR=success docker compose up -d worker
```

Unresolved and overdue:

```sh
MOCK_BEHAVIOR=unresolved PAYMENT_DEADLINE=5s docker compose up -d api worker
# Acquire a fresh seat, start payment, wait ~10s.
docker compose logs worker | grep -i overdue
# Expected: the attempt stays pending, the seat stays held, and the worker logs
# `payments overdue` on every settlement tick.
MOCK_BEHAVIOR=success PAYMENT_DEADLINE=15m docker compose up -d api worker
```

## Inspect PostgreSQL

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, seat_id, status, expires_at FROM holds ORDER BY created_at DESC LIMIT 5;"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, status, current_hold_id, hold_expires_at FROM seats WHERE id = '$SEAT_ID';"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, topic, payload, published_at FROM outbox ORDER BY id DESC LIMIT 5;"
```

Payment records and the mock provider:

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, hold_id, status, attempts, last_error FROM payment_attempts ORDER BY created_at DESC LIMIT 5;"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, amount_cents, currency FROM orders WHERE seat_id = '$SEAT_ID';"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT idempotency_key, status, amount_cents, currency FROM mockpay.charges ORDER BY created_at DESC LIMIT 5;"

# A succeeded attempt must have an order.
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT p.id FROM payment_attempts p
     LEFT JOIN orders o ON o.payment_attempt_id = p.id
    WHERE p.status = 'succeeded' AND o.id IS NULL;"

# An order must match its attempt's user, amount and currency.
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT o.id FROM orders o
     JOIN payment_attempts p ON p.id = o.payment_attempt_id
    WHERE o.user_id <> p.user_id
       OR o.amount_cents <> p.amount_cents
       OR o.currency <> p.currency;"
```

Both payment invariant queries should return zero rows.

The worker relay publishes outbox rows within about a second; `published_at` is
NULL only while a row is waiting for (or retrying) its handler.

The two data-level checks behind the invariants should return **zero rows**:

```sh
# Invariant 1: no seat ever has two live/sold holds
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT seat_id, count(*) FROM holds
    WHERE status IN ('active','payment_pending','confirmed')
    GROUP BY seat_id HAVING count(*) > 1;"

# Seat expiry must agree with its hold
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT s.id FROM seats s JOIN holds h ON h.id = s.current_hold_id
    WHERE (h.status = 'active' AND s.hold_expires_at IS DISTINCT FROM h.expires_at)
       OR (h.status IN ('payment_pending','confirmed') AND s.hold_expires_at IS NOT NULL);"
```

Check the worker's checker directly by restarting it and watching logs. It only
logs on violations or errors, so silence is success:

```sh
docker compose restart worker
docker compose logs -f worker
```

## Watch the sweeper expire a hold

Take a fresh hold on another seat, then backdate it in PostgreSQL:

```sh
SEAT_ID=0199a000-0000-7000-8000-000000000203
code=$(req POST "/events/$EVENT_ID/holds" "$USER_A" "{\"seat_id\":\"$SEAT_ID\"}")
expect "$code" 201 "hold for expiry demo"
HOLD_ID=$(holdid)

docker compose exec -T postgres psql -U tickets -d tickets -c "
BEGIN;
UPDATE holds SET expires_at = clock_timestamp() - interval '1 second'
 WHERE id = '$HOLD_ID';
UPDATE seats SET hold_expires_at = (SELECT expires_at FROM holds WHERE id = '$HOLD_ID')
 WHERE current_hold_id = '$HOLD_ID';
COMMIT;"
```

Wait up to 6 seconds for the sweeper tick, then inspect:

```sh
sleep 6
req GET "/holds/$HOLD_ID" "$USER_A"; echo; body
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT status, current_hold_id, hold_expires_at FROM seats WHERE id = '$SEAT_ID';"
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT status FROM holds WHERE id = '$HOLD_ID';"
```

Expected: hold `expired`, seat `available`, both seat columns NULL, and one new
`seat.changed` outbox row. The sweeper never touches `payment_pending`.

## Prove concurrency yourself with curl

Use a fresh seat and 20 different users:

```sh
SEAT_ID=0199a000-0000-7000-8000-000000000204
{ for i in $(seq 1 20); do
    U=$(python3 -c 'import uuid; print(uuid.uuid4())')
    curl -s -o /dev/null -w '%{http_code}\n' -X POST "$BASE/events/$EVENT_ID/holds" \
      -H "X-User-ID: $U" -H 'Content-Type: application/json' \
      -d "{\"seat_id\":\"$SEAT_ID\"}" &
  done; wait; } | sort | uniq -c
```

Expected exactly:

```text
  19 409
   1 201
```

Then confirm one live hold:

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT count(*) FROM holds WHERE seat_id = '$SEAT_ID' AND status = 'active';"
```

Expected: `1`.

## Redis cache, rate limiting and invalidation

If you are in a new shell, redefine the variables from the hold script
(`BASE`, `EVENT_ID`, `SEAT_ID`, `USER_ID`).

Populate and inspect the seat-map cache:

```sh
curl -sS "$BASE/events/$EVENT_ID/seats" > /dev/null
docker compose exec -T redis redis-cli --scan --pattern 'seatmap:*'
```

You should see `seatmap:{event-id}`. The key has a 5-second TTL. A miss populates
it from PostgreSQL, and `singleflight` collapses concurrent misses.

Take a hold and watch the relay invalidate it:

```sh
HOLD_ID=$(curl -sS -X POST "$BASE/events/$EVENT_ID/holds" \
  -H "X-User-ID: $USER_ID" -H 'Content-Type: application/json' \
  -d "{\"seat_id\":\"$SEAT_ID\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["hold"]["id"])')

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, published_at FROM outbox
    WHERE payload->>'seat_id' = '$SEAT_ID'
    ORDER BY id DESC LIMIT 1;"
docker compose exec -T redis redis-cli --scan --pattern "seatmap:$EVENT_ID"
```

The worker relay runs every second: it deletes the key and marks the outbox row
published.

Rate limiting:

```sh
HOLD_RATE_LIMIT=2 docker compose up -d api
# Same user, three different available seats: 201, 201, 429 rate_limited.
# A different user is still allowed.
HOLD_RATE_LIMIT=60 docker compose up -d api
```

Redis stopped (invariant 10):

```sh
docker compose stop redis
curl -sS -i "$BASE/readyz"     # 200, "redis":"unavailable"
# Take a hold and start a payment; both still work.
docker compose start redis
```

## Metrics and tracing

```sh
curl -s http://localhost:8080/metrics | grep -E \
  'hold_attempts_total|payments_total|seatmap_cache_total|redis_errors_total'
curl -s http://localhost:9090/metrics | grep -E \
  'payments_overdue|outbox_lag_seconds|invariant_violations_total|pgxpool_acquired_conns'
```

- Take a hold and make a conflicting hold: `hold_attempts_total{result="acquired"}`
  and `{result="conflict"}` increment.
- Start a payment: `payments_total{result="started"}` increments; after the
  worker settles, the worker registry shows `payments_total{result="succeeded"}`.
- Read the seat map twice: `seatmap_cache_total{result="miss"}` then `hit`.
- Stop Redis and make a request: `redis_errors_total` increments; readiness stays
  200 with Redis unavailable.

Tracing:

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, traceparent FROM payment_attempts ORDER BY created_at DESC LIMIT 3;"
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, topic, traceparent FROM outbox ORDER BY id DESC LIMIT 5;"
```

HTTP requests, pgx queries and provider calls emit spans. Payment attempts and
outbox rows store the `traceparent`, and the worker resumes it during settlement
and outbox handling. With no collector configured, trace ids are still generated
and propagated. To export, run an OTLP HTTP collector and set
`OTEL_EXPORTER_OTLP_ENDPOINT` for both the API and worker; no collector is
bundled in Compose.

## Run the hard tests individually

```sh
go test -race -count=1 -run 'TestInvariant01' -v ./test/integration/...
go test -race -count=1 -run 'TestInvariant02|TestInvariant03|TestInvariant05' -v ./test/integration/...
go test -race -count=1 -run 'TestHoldTransitionAgreement' -v ./test/integration/...
go test -race -count=1 -run 'TestFault05|TestFault09' -v ./test/integration/...
go test -race -count=1 -run 'TestExpirySweeper|TestInvariantCheckerDetectsMismatch' -v ./test/integration/...
go test -race -count=1 -run 'TestPaymentFlowSuccess|TestInvariant04|TestInvariant06|TestInvariant07|TestFault07|TestFault11' -v ./test/integration/...
go test -race -count=1 -run 'TestFault04|TestFault10' -v ./test/integration/...
go test -race -count=1 -run 'TestInvariant08|TestFault08' -v ./test/integration/...
go test -race -count=1 -run 'TestFault01|TestFault03|TestInvariant09|TestC3' -v ./test/integration/...
go test -race -count=1 -run 'TestSeatMap|TestRateLimit|TestInvariant10|TestMetricsEndpoint' -v ./test/integration/...
```

What each proves:

- `TestInvariant01_*`: C1 (1,000 goroutines, one seat) and C2 (10,000 requests,
  100 seats, exactly 100 holds).
- `TestHoldTransitionAgreement`: all 36 status pairs through both the pure domain
  function and the SQL guard; both must agree.
- `TestFault09_ConcurrentDuplicateAcquire`: same user, same seat, simultaneous
  requests produce one 201, one 200, the same hold id, and no 409.
- `TestFault05_ConnectionCutMidTransaction`: locks the seat row from an admin
  transaction, starts a real acquire, finds the blocked backend in
  `pg_stat_activity`, terminates it with `pg_terminate_backend`, then asserts
  500, full rollback, and a clean invariant check.
- `TestInvariantCheckerDetectsMismatch`: intentionally corrupts a seat's expiry
  and proves the checker reports it.
- `TestInvariant06_OneChargePerHold`: 50 concurrent requests with one
  idempotency key produce one attempt; a different key gets
  `payment_in_progress`; one charge and one order.
- `TestInvariant07_SeatSoldAtMostOnce`: duplicate settlement and worker retries
  create one order.
- `TestInvariant08_AmbiguousPaymentNeverReleasesSeat`: `timeout_after_charge`
  leaves the attempt pending and the seat held; the next check finds the charge
  and sells the seat.
- `TestFault01`, `TestFault03`, `TestInvariant09`: worker crash and restart
  recovery using leases and the provider's stable idempotency key.
- `TestFault04`, `TestFault10`: a decline releases the seat; a second key after
  the decline gets `hold_not_active`.
- `TestFault07`: a payment waiting on the hold row lock past expiry gets 409; a
  payment whose guard ran before expiry stays valid even with a late commit.
- `TestFault08`: `unresolved` stays pending and overdue while the seat stays
  held.
- `TestFault11`: a dependent update matching zero rows rolls back with an
  internal error and no partial state.
- `TestC3_RandomizedMix`: randomized hold/release/pay traffic with the
  settlement and sweep workers running; the checker ends clean.
- `TestSeatMapCacheHitAndRelayInvalidation`: populates Redis, then proves the
  outbox relay deletes the key and marks the row published.
- `TestSeatMapServedFromCacheUnderLoad`: 50 concurrent readers see the cached
  price after PostgreSQL changes, proving the cache is serving them.
- `TestRateLimitHoldRequests`: the per-user limit returns 429 while another user
  proceeds.
- `TestInvariant10_RedisStoppedFailsOpen`: with Redis stopped, the seat map falls
  back to PostgreSQL and holds/payments still work.
- `TestMetricsEndpoint`: scrapes `/metrics` and checks hold/payment series.

Fuzz the transition state machine:

```sh
go test -fuzz=FuzzHoldTransitions -fuzztime=30s ./internal/domain/reservation
```

## CI and operational checks

- Push or open a PR: `.github/workflows/ci.yml` runs lint and
  `go test -race ./...` on Ubuntu with Docker available for Testcontainers. It
  installs Go and golangci-lint from `.tool-versions` via asdf.
- Readiness behavior:

```sh
docker compose stop postgres
curl -i http://localhost:8080/readyz     # 503 {"status":"unavailable"}
docker compose start postgres
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/readyz)" = 200 ]; do sleep 1; done
```

- Logs: `docker compose logs -f api worker`. API logs are JSON with
  `request_id`, `method`, `path`, `status`, and `duration_ms`; match
  `request_id` to the `X-Request-Id` response header.

## Reset and troubleshooting

- Reset everything: `docker compose down -v && docker compose up -d --build`.
- If `go version` is not 1.27.3 while `.tool-versions` says it is, your shell has
  a stale `GOROOT` from an older asdf install. Fix with
  `unset GOROOT GOPATH GOBIN` or a new shell.
- If Testcontainers fails, verify Docker works first:
  `docker run --rm hello-world`.
- If `readyz` is 503, check `docker compose ps` and
  `docker compose logs postgres api`.
- Ports: Compose uses 5432 (PostgreSQL) and 8080 (API). Testcontainers uses
  random host ports, so it can run alongside the Compose stack.

## What "working fine" looks like

- `make test-unit`, `make test`, and `make lint` are green.
- One 201 per seat; duplicate same user is 200 with the same id; everyone else
  gets 409 `seat_unavailable`.
- Foreign hold reads/releases return 404; a second release returns 409
  `hold_not_active`.
- Backdated holds become `expired`, their seats become `available`, and outbox
  rows appear.
- Twenty concurrent buyers produce exactly one 201.
- Starting a payment returns 202, polling reaches `confirmed`/`succeeded`, the
  same key returns 200 with the same attempt, and a different key after the
  final result returns 409 `hold_not_active`.
- A decline ends in `payment_failed` and an available seat; a timeout after
  charge ends in `succeeded` and a sold seat, never a released seat.
- An unresolved payment past its deadline logs `payments overdue` and keeps the
  seat held.
- There is exactly one `mockpay.charges` row and one order per successful hold.
- The seat-map cache key appears after a read and disappears after a
  `seat.changed` outbox row is relayed; outbox rows end with `published_at` set.
- A third hold from a user with `HOLD_RATE_LIMIT=2` returns 429 `rate_limited`.
- With Redis stopped, `/readyz` stays 200 with `"redis":"unavailable"`, and holds
  and payments still work.
- The API and worker `/metrics` endpoints expose the documented series; the
  worker shows `pgxpool_acquired_conns` and `payments_overdue`.
- Payment attempts and outbox rows have non-null `traceparent` values.
- No `invariant violation` logs; the invariant SQL checks return zero rows.

The executable specifications are `test/integration/agreement_test.go`,
`test/integration/concurrency_test.go`, and `test/integration/faults_test.go`;
the design rationale is in `docs/adr/0001` and `docs/adr/0002`.
