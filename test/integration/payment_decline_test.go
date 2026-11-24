package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestFault04_DeclineReleasesSeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userA := uuid.NewString()
	userB := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userA)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userA, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	provider := newMockProvider(t, "decline")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	status, data := doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, userA, "")
	if status != http.StatusOK {
		t.Fatalf("get hold status = %d: %s", status, data)
	}
	final := decodeHold(t, data)
	if final.Hold.Status != "payment_failed" {
		t.Errorf("hold status = %q, want payment_failed", final.Hold.Status)
	}
	if final.Payment == nil || final.Payment.Status != "failed" {
		t.Errorf("payment = %+v, want failed", final.Payment)
	}

	assertAttemptStatus(t, pool, attemptID, "failed")
	assertSeatState(t, pool, seatID, seatState{Status: "available", ExpiryNull: true})
	if orders := orderCount(t, pool, seatID); orders != 0 {
		t.Errorf("orders = %d, want 0", orders)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userB,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire after decline status = %d, want 201: %s", status, data)
	}

	checkInvariants(t, pool)
}

func TestFault10_SecondKeyAfterDecline(t *testing.T) {
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

	provider := newMockProvider(t, "decline")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}
	if got := attemptStatus(t, pool, attemptID); got != "failed" {
		t.Fatalf("attempt status = %q, want failed", got)
	}

	status, data, err := rawStartPayment(ctx, server.URL, holdID, userID, "key-2")
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("second key status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if code := decodeError(t, data).Error.Code; code != "hold_not_active" {
		t.Errorf("error code = %q, want hold_not_active", code)
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	status, retry := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusOK {
		t.Fatalf("same key status = %d, want %d", status, http.StatusOK)
	}
	if retry.Payment == nil || retry.Payment.ID != attemptID {
		t.Errorf("same key payment = %+v, want attempt %s", retry.Payment, attemptID)
	}

	checkInvariants(t, pool)
}
