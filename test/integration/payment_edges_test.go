package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestPaymentRejectsForeignAndReleasedHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	owner := uuid.NewString()
	other := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, owner)

	status, data, err := rawStartPayment(ctx, server.URL, holdID, other, "key-1")
	if err != nil {
		t.Fatalf("foreign payment: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("foreign status = %d, want %d: %s", status, http.StatusNotFound, data)
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}

	status, data = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, owner, "")
	if status != http.StatusNoContent {
		t.Fatalf("release status = %d: %s", status, data)
	}

	status, data, err = rawStartPayment(ctx, server.URL, holdID, owner, "key-1")
	if err != nil {
		t.Fatalf("released payment: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("released status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if code := decodeError(t, data).Error.Code; code != "hold_not_active" {
		t.Errorf("error code = %q, want hold_not_active", code)
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}

	checkInvariants(t, pool)
}

func TestPaymentIdempotencyKeyReusedAcrossHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	userID := uuid.NewString()

	holdOne := acquireHoldHTTP(t, server.URL, eventID, seatIDs[0], userID)
	holdTwo := acquireHoldHTTP(t, server.URL, eventID, seatIDs[1], userID)

	status, envelope := startPaymentHTTP(t, server.URL, holdOne, userID, "shared-key")
	if status != http.StatusAccepted {
		t.Fatalf("first payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	status, data, err := rawStartPayment(ctx, server.URL, holdTwo, userID, "shared-key")
	if err != nil {
		t.Fatalf("reused key: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("reused key status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if code := decodeError(t, data).Error.Code; code != "idempotency_key_reused" {
		t.Errorf("error code = %q, want idempotency_key_reused", code)
	}

	if attempts := attemptCount(t, pool, holdOne); attempts != 1 {
		t.Errorf("hold one attempts = %d, want 1", attempts)
	}
	if attempts := attemptCount(t, pool, holdTwo); attempts != 0 {
		t.Errorf("hold two attempts = %d, want 0", attempts)
	}
	if got := attemptStatus(t, pool, attemptID); got != "created" {
		t.Errorf("attempt status = %q, want created", got)
	}

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	checkInvariants(t, pool)
}

func TestPaymentUsesHoldPriceSnapshot(t *testing.T) {
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

	var holdAmount int64
	var holdCurrency string
	if err := pool.QueryRow(ctx,
		"SELECT amount_cents, currency FROM holds WHERE id = $1::uuid", holdID,
	).Scan(&holdAmount, &holdCurrency); err != nil {
		t.Fatalf("read hold amount: %v", err)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE seats SET price_cents = 9999 WHERE id = $1::uuid", seatID,
	); err != nil {
		t.Fatalf("change seat price: %v", err)
	}

	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	attemptAmountCents, attemptCurrency := attemptAmount(t, pool, attemptID)
	if attemptAmountCents != holdAmount || attemptCurrency != holdCurrency {
		t.Errorf("attempt amount = %d %s, want hold snapshot %d %s",
			attemptAmountCents, attemptCurrency, holdAmount, holdCurrency)
	}

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	var orderAmount int64
	var orderCurrency string
	if err := pool.QueryRow(ctx,
		"SELECT amount_cents, currency FROM orders WHERE seat_id = $1::uuid", seatID,
	).Scan(&orderAmount, &orderCurrency); err != nil {
		t.Fatalf("read order amount: %v", err)
	}
	if orderAmount != holdAmount || orderCurrency != holdCurrency {
		t.Errorf("order amount = %d %s, want hold snapshot %d %s",
			orderAmount, orderCurrency, holdAmount, holdCurrency)
	}

	checkInvariants(t, pool)
}
