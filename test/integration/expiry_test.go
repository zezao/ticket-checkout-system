package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

func TestExpirySweeper(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	eventID := insertEvent(t, pool)
	expiredSeatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)
	pendingSeatID := insertSeat(t, pool, eventID, "GA", "A", 2, 8500)

	userID := uuid.NewString()
	expiredHoldID := uuid.NewString()
	pendingHoldID := uuid.NewString()
	past := time.Now().Add(-time.Minute).UTC()
	future := time.Now().Add(10 * time.Minute).UTC()

	insertHold(t, pool, expiredHoldID, expiredSeatID, userID, reservation.HoldActive, past)
	holdSeat(t, pool, expiredSeatID, expiredHoldID, &past)
	insertHold(t, pool, pendingHoldID, pendingSeatID, userID, reservation.HoldPaymentPending, future)
	holdSeat(t, pool, pendingSeatID, pendingHoldID, nil)

	store := pgadapter.NewHoldStore(pool)
	expired, err := store.ExpireBatch(ctx, 100)
	if err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	if expired < 1 {
		t.Errorf("expired = %d, want at least 1", expired)
	}

	if got := holdStatus(t, pool, expiredHoldID); got != "expired" {
		t.Errorf("expired hold status = %q, want expired", got)
	}
	assertSeatState(t, pool, expiredSeatID, seatState{Status: "available", ExpiryNull: true})

	if got := holdStatus(t, pool, pendingHoldID); got != "payment_pending" {
		t.Errorf("pending hold status = %q, want payment_pending", got)
	}
	assertSeatState(t, pool, pendingSeatID, seatState{
		Status:        "held",
		CurrentHoldID: pendingHoldID,
		ExpiryNull:    true,
	})

	var outboxRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		 WHERE topic = 'seat.changed'
		   AND payload->>'seat_id' = $1`, expiredSeatID,
	).Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxRows != 1 {
		t.Errorf("outbox rows = %d, want 1", outboxRows)
	}

	expired, err = store.ExpireBatch(ctx, 100)
	if err != nil {
		t.Fatalf("second ExpireBatch: %v", err)
	}
	if expired != 0 {
		t.Errorf("second sweep expired = %d, want 0", expired)
	}

	checkInvariants(t, pool)
}

func TestInvariant02_SweeperNeverReleasesSoldSeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	userID := uuid.NewString()
	holdID := uuid.NewString()
	attemptID := uuid.NewString()
	orderID := uuid.NewString()
	future := time.Now().Add(10 * time.Minute).UTC()

	insertHold(t, pool, holdID, seatID, userID, reservation.HoldConfirmed, future)
	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET status = 'sold', current_hold_id = $1::uuid, hold_expires_at = NULL
		 WHERE id = $2::uuid`, holdID, seatID,
	); err != nil {
		t.Fatalf("sell seat: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_attempts (
		    id, hold_id, user_id, idempotency_key, amount_cents, currency,
		    status, deadline_at
		) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 8500, 'USD', 'succeeded', $5)`,
		attemptID, holdID, userID, uuid.NewString(), future,
	); err != nil {
		t.Fatalf("insert payment attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO orders (id, hold_id, seat_id, payment_attempt_id, user_id, amount_cents, currency)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, 8500, 'USD')`,
		orderID, holdID, seatID, attemptID, userID,
	); err != nil {
		t.Fatalf("insert order: %v", err)
	}

	store := pgadapter.NewHoldStore(pool)
	if _, err := store.ExpireBatch(ctx, 100); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}

	assertSeatState(t, pool, seatID, seatState{
		Status:        "sold",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	if got := holdStatus(t, pool, holdID); got != "confirmed" {
		t.Errorf("hold status = %q, want confirmed", got)
	}

	var orders int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM orders WHERE seat_id = $1::uuid", seatID,
	).Scan(&orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}

	checkInvariants(t, pool)
}

func TestInvariant05_PaymentPendingSeatCannotBeTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	owner := uuid.NewString()
	other := uuid.NewString()
	holdID := uuid.NewString()
	future := time.Now().Add(10 * time.Minute).UTC()

	insertHold(t, pool, holdID, seatID, owner, reservation.HoldPaymentPending, future)
	holdSeat(t, pool, seatID, holdID, nil)

	store := pgadapter.NewHoldStore(pool)
	if _, err := store.ExpireBatch(ctx, 100); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", other,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("other user status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if got := decodeError(t, data).Error.Code; got != "seat_unavailable" {
		t.Errorf("error code = %q, want seat_unavailable", got)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", owner,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("owner status = %d, want %d: %s", status, http.StatusOK, data)
	}
	body := decodeHold(t, data)
	if body.Hold.ID != holdID {
		t.Errorf("hold id = %q, want %q", body.Hold.ID, holdID)
	}
	if body.Hold.Status != "payment_pending" {
		t.Errorf("hold status = %q, want payment_pending", body.Hold.Status)
	}

	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	if got := holdStatus(t, pool, holdID); got != "payment_pending" {
		t.Errorf("hold status = %q, want payment_pending", got)
	}

	checkInvariants(t, pool)
}

type seatState struct {
	Status        string
	CurrentHoldID string
	ExpiryNull    bool
}

func assertSeatState(t *testing.T, pool *pgxpool.Pool, seatID string, want seatState) {
	t.Helper()

	var got seatState
	if err := pool.QueryRow(context.Background(), `
		SELECT status::text,
		       COALESCE(current_hold_id::text, ''),
		       hold_expires_at IS NULL
		  FROM seats
		 WHERE id = $1::uuid`, seatID,
	).Scan(&got.Status, &got.CurrentHoldID, &got.ExpiryNull); err != nil {
		t.Fatalf("read seat state: %v", err)
	}

	if got != want {
		t.Errorf("seat state = %+v, want %+v", got, want)
	}
}

func holdStatus(t *testing.T, pool *pgxpool.Pool, holdID string) string {
	t.Helper()

	var status string
	if err := pool.QueryRow(context.Background(),
		"SELECT status::text FROM holds WHERE id = $1::uuid", holdID,
	).Scan(&status); err != nil {
		t.Fatalf("read hold status: %v", err)
	}
	return status
}
