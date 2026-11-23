package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type acquireOutcome struct {
	status int
	holdID string
	code   string
	err    error
}

func TestFault09_ConcurrentDuplicateAcquire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	results := make(chan acquireOutcome, 2)
	start := make(chan struct{})

	for i := 0; i < 2; i++ {
		go func() {
			<-start

			status, data, err := rawRequest(ctx, http.MethodPost,
				server.URL+"/events/"+eventID+"/holds", userID,
				`{"seat_id":"`+seatID+`"}`)
			outcome := acquireOutcome{status: status, err: err}
			if err == nil {
				switch status {
				case http.StatusOK, http.StatusCreated:
					var body holdEnvelopeBody
					if decodeErr := json.Unmarshal(data, &body); decodeErr != nil {
						outcome.err = decodeErr
					} else {
						outcome.holdID = body.Hold.ID
					}
				default:
					outcome.code = errorCode(data)
				}
			}
			results <- outcome
		}()
	}

	close(start)

	first := <-results
	second := <-results

	for _, outcome := range []acquireOutcome{first, second} {
		if outcome.err != nil {
			t.Fatalf("acquire failed: %v", outcome.err)
		}
	}

	if first.status > second.status {
		first, second = second, first
	}
	if first.status != http.StatusOK || second.status != http.StatusCreated {
		t.Fatalf("statuses = %d and %d (codes %q, %q), want 200 and 201",
			first.status, second.status, first.code, second.code)
	}
	if first.holdID == "" || first.holdID != second.holdID {
		t.Fatalf("hold ids = %q and %q, want the same non-empty id", first.holdID, second.holdID)
	}

	checkInvariants(t, pool)
}

func TestFault05_ConnectionCutMidTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	oldUser := uuid.NewString()
	newUser := uuid.NewString()
	oldHoldID := uuid.NewString()
	past := time.Now().Add(-time.Minute).UTC()

	insertHold(t, pool, oldHoldID, seatID, oldUser, reservation.HoldActive, past)
	holdSeat(t, pool, seatID, oldHoldID, &past)

	adminConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire admin connection: %v", err)
	}
	defer adminConn.Release()

	adminTx, err := adminConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin admin transaction: %v", err)
	}
	defer func() { _ = adminTx.Rollback(ctx) }()

	var lockedSeatID string
	if err := adminTx.QueryRow(ctx,
		"SELECT id::text FROM seats WHERE id = $1::uuid FOR UPDATE", seatID,
	).Scan(&lockedSeatID); err != nil {
		t.Fatalf("lock seat: %v", err)
	}

	outcome := make(chan acquireOutcome, 1)
	go func() {
		status, data, err := rawRequest(ctx, http.MethodPost,
			server.URL+"/events/"+eventID+"/holds", newUser,
			`{"seat_id":"`+seatID+`"}`)
		outcome <- acquireOutcome{status: status, holdID: errorCode(data), err: err}
	}()

	pid := findBlockedAcquire(t, ctx, pool)

	var terminated bool
	if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}
	if !terminated {
		t.Fatalf("backend %d was not terminated", pid)
	}

	select {
	case result := <-outcome:
		if result.err != nil {
			t.Fatalf("acquire request: %v", result.err)
		}
		if result.status != http.StatusInternalServerError {
			t.Fatalf("status = %d (code %q), want %d", result.status, result.holdID, http.StatusInternalServerError)
		}
		if result.holdID != "internal_error" {
			t.Errorf("error code = %q, want internal_error", result.holdID)
		}
	case <-ctx.Done():
		t.Fatal("acquire request did not return after the connection was cut")
	}

	if err := adminTx.Rollback(ctx); err != nil {
		t.Fatalf("release admin lock: %v", err)
	}

	if got := holdStatus(t, pool, oldHoldID); got != "active" {
		t.Errorf("old hold status = %q, want active (the interrupted transaction should roll back)", got)
	}
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: oldHoldID,
	})

	var holds int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM holds WHERE seat_id = $1::uuid", seatID,
	).Scan(&holds); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	if holds != 1 {
		t.Errorf("holds = %d, want 1 (no partial acquire)", holds)
	}

	checkInvariants(t, pool)

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", newUser,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire after recovery status = %d: %s", status, data)
	}
	newHoldID := decodeHold(t, data).Hold.ID

	if got := holdStatus(t, pool, oldHoldID); got != "expired" {
		t.Errorf("old hold status = %q, want expired", got)
	}
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: newHoldID,
	})

	checkInvariants(t, pool)
}

func findBlockedAcquire(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := pool.QueryRow(ctx, `
			SELECT pid
			  FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND pid <> pg_backend_pid()
			   AND wait_event_type = 'Lock'
			   AND query ILIKE '%update seats%'
			   AND query ILIKE '%current_hold_id%'
			 LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("find blocked acquire: %v", err)
		}

		select {
		case <-ctx.Done():
			t.Fatal("context ended while waiting for the blocked acquire")
		case <-time.After(20 * time.Millisecond):
		}
	}

	t.Fatal("no blocked acquire transaction appeared")
	return 0
}

func errorCode(data []byte) string {
	var body errorDTO
	if err := json.Unmarshal(data, &body); err != nil {
		return ""
	}
	return body.Error.Code
}
