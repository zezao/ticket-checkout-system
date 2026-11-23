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
| `make test-unit` | Config, handlers, use cases, domain transitions, sweeper/checker loops. No Docker needed. |
| `make test` | Everything above plus real HTTP + real PostgreSQL: hold lifecycle, ownership, expiry, all 36 transition pairs, C1/C2, fault 5 and 9. |
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

## Inspect PostgreSQL

```sh
docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, seat_id, status, expires_at FROM holds ORDER BY created_at DESC LIMIT 5;"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, status, current_hold_id, hold_expires_at FROM seats WHERE id = '$SEAT_ID';"

docker compose exec -T postgres psql -U tickets -d tickets -c \
  "SELECT id, topic, payload, published_at FROM outbox ORDER BY id DESC LIMIT 5;"
```

`published_at` is NULL until the Phase 4 outbox relay; that is expected.

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

## Run the hard tests individually

```sh
go test -race -count=1 -run 'TestInvariant01' -v ./test/integration/...
go test -race -count=1 -run 'TestInvariant02|TestInvariant03|TestInvariant05' -v ./test/integration/...
go test -race -count=1 -run 'TestHoldTransitionAgreement' -v ./test/integration/...
go test -race -count=1 -run 'TestFault05|TestFault09' -v ./test/integration/...
go test -race -count=1 -run 'TestExpirySweeper|TestInvariantCheckerDetectsMismatch' -v ./test/integration/...
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
- No `invariant violation` logs; the invariant SQL checks return zero rows.

The executable specifications are `test/integration/agreement_test.go`,
`test/integration/concurrency_test.go`, and `test/integration/faults_test.go`;
the design rationale is in `docs/adr/0001` and `docs/adr/0002`.
