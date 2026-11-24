package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
)

func TestInvariant08_AmbiguousPaymentNeverReleasesSeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	provider := newMockProvider(t, "timeout_after_charge")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	assertAttemptStatus(t, pool, attemptID, "pending")
	assertHoldStatus(t, pool, holdID, "payment_pending")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	if orders := orderCount(t, pool, seatID); orders != 0 {
		t.Errorf("orders = %d, want 0", orders)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}

	makeAttemptDue(t, pool, attemptID)
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	assertAttemptStatus(t, pool, attemptID, "succeeded")
	assertHoldStatus(t, pool, holdID, "confirmed")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "sold",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}

	checkInvariants(t, pool)
}

func TestFault08_UnresolvedPaymentStaysPendingAndOverdue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServerWithPaymentDeadline(t, pool, 300*time.Millisecond)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	time.Sleep(400 * time.Millisecond)

	provider := newMockProvider(t, "unresolved")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	assertAttemptStatus(t, pool, attemptID, "pending")
	assertHoldStatus(t, pool, holdID, "payment_pending")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	if orders := orderCount(t, pool, seatID); orders != 0 {
		t.Errorf("orders = %d, want 0", orders)
	}

	store := pgadapter.NewPaymentStore(pool)
	overdue, err := store.CountOverdue(ctx)
	if err != nil {
		t.Fatalf("count overdue: %v", err)
	}
	if overdue != 1 {
		t.Errorf("overdue = %d, want 1", overdue)
	}

	makeAttemptDue(t, pool, attemptID)
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	assertAttemptStatus(t, pool, attemptID, "pending")
	assertHoldStatus(t, pool, holdID, "payment_pending")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	overdue, err = store.CountOverdue(ctx)
	if err != nil {
		t.Fatalf("count overdue: %v", err)
	}
	if overdue != 1 {
		t.Errorf("overdue after retry = %d, want 1", overdue)
	}

	checkInvariants(t, pool)
}

func makeAttemptDue(t *testing.T, pool *pgxpool.Pool, attemptID string) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), `
		UPDATE payment_attempts
		   SET next_check_at = clock_timestamp()
		 WHERE id = $1::uuid`, attemptID); err != nil {
		t.Fatalf("make attempt due: %v", err)
	}
}
