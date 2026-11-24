package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

func TestFault01_LeaseRecoveryAfterWorkerCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

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

	store := pgadapter.NewPaymentStore(pool)
	provider := newMockProvider(t, "success")
	settlement := app.NewSettlement(store, provider, 2*time.Second)

	const lease = 500 * time.Millisecond

	first, err := store.ClaimDue(ctx, lease, 100)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	claimed := findAttempt(first, attemptID)
	if claimed == nil {
		t.Fatal("first claim did not include the attempt")
	}

	early, err := store.ClaimDue(ctx, lease, 100)
	if err != nil {
		t.Fatalf("early claim: %v", err)
	}
	if findAttempt(early, attemptID) != nil {
		t.Fatal("attempt was claimable before its lease expired")
	}

	time.Sleep(lease + 200*time.Millisecond)

	again, err := store.ClaimDue(ctx, lease, 100)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	recovered := findAttempt(again, attemptID)
	if recovered == nil {
		t.Fatal("attempt was not claimable after its lease expired")
	}
	if err := settlement.Settle(ctx, *recovered); err != nil {
		t.Fatalf("settle recovered attempt: %v", err)
	}

	assertAttemptStatus(t, pool, attemptID, "succeeded")
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

func TestFault03_RecoveryAfterChargeBeforeSettle(t *testing.T) {
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

	provider := newMockProvider(t, "success")
	amount, currency := attemptAmount(t, pool, attemptID)

	if _, err := provider.CreateCharge(ctx, app.ChargeRequest{
		IdempotencyKey: attemptID,
		AmountCents:    amount,
		Currency:       currency,
	}); err != nil {
		t.Fatalf("direct charge: %v", err)
	}
	assertAttemptStatus(t, pool, attemptID, "created")

	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	assertAttemptStatus(t, pool, attemptID, "succeeded")
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

func TestInvariant09_PaymentStateSurvivesWorkerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

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

	provider := newMockProvider(t, "success")

	const lease = 300 * time.Millisecond

	workerAStore := pgadapter.NewPaymentStore(pool)
	claimedByA, err := workerAStore.ClaimDue(ctx, lease, 100)
	if err != nil {
		t.Fatalf("worker A claim: %v", err)
	}
	if findAttempt(claimedByA, attemptID) == nil {
		t.Fatal("worker A did not claim the attempt")
	}

	time.Sleep(lease + 200*time.Millisecond)

	workerBStore := pgadapter.NewPaymentStore(pool)
	workerBSettlement := app.NewSettlement(workerBStore, provider, 2*time.Second)
	claimedByB, err := workerBStore.ClaimDue(ctx, lease, 100)
	if err != nil {
		t.Fatalf("worker B claim: %v", err)
	}
	recovered := findAttempt(claimedByB, attemptID)
	if recovered == nil {
		t.Fatal("worker B did not recover the attempt")
	}
	if err := workerBSettlement.Settle(ctx, *recovered); err != nil {
		t.Fatalf("worker B settle: %v", err)
	}

	assertAttemptStatus(t, pool, attemptID, "succeeded")
	assertHoldStatus(t, pool, holdID, "confirmed")
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}

	checkInvariants(t, pool)
}

func findAttempt(attempts []payment.Attempt, id string) *payment.Attempt {
	for i := range attempts {
		if attempts[i].ID == id {
			return &attempts[i]
		}
	}
	return nil
}

func attemptAmount(t *testing.T, pool *pgxpool.Pool, attemptID string) (int64, string) {
	t.Helper()

	var (
		amount   int64
		currency string
	)
	if err := pool.QueryRow(context.Background(),
		"SELECT amount_cents, currency FROM payment_attempts WHERE id = $1::uuid", attemptID,
	).Scan(&amount, &currency); err != nil {
		t.Fatalf("read attempt amount: %v", err)
	}
	return amount, currency
}
