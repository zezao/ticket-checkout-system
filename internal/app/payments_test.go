package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

type stubPaymentStore struct {
	startParams StartPaymentParams
	startResult payment.Attempt
	startErr    error

	gotHoldID string
	gotUserID string
	getResult payment.Attempt
	getFound  bool
	getErr    error
}

func (s *stubPaymentStore) Start(_ context.Context, params StartPaymentParams) (payment.Attempt, error) {
	s.startParams = params
	return s.startResult, s.startErr
}

func (s *stubPaymentStore) GetForHold(_ context.Context, holdID, userID string) (payment.Attempt, bool, error) {
	s.gotHoldID = holdID
	s.gotUserID = userID
	return s.getResult, s.getFound, s.getErr
}

func TestPaymentsStartBuildsParams(t *testing.T) {
	t.Parallel()

	store := &stubPaymentStore{startResult: payment.Attempt{ID: "attempt-1"}}
	payments := NewPayments(store, func() (string, error) {
		return "generated-attempt", nil
	}, 15*time.Minute)

	attempt, err := payments.Start(context.Background(), StartPaymentRequest{
		HoldID:         "hold-1",
		UserID:         "user-1",
		IdempotencyKey: "key-1",
		Traceparent:    "trace-1",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if attempt.ID != "attempt-1" {
		t.Errorf("attempt id = %q", attempt.ID)
	}

	want := StartPaymentParams{
		AttemptID:      "generated-attempt",
		HoldID:         "hold-1",
		UserID:         "user-1",
		IdempotencyKey: "key-1",
		Traceparent:    "trace-1",
		Deadline:       15 * time.Minute,
	}
	if store.startParams != want {
		t.Errorf("params = %+v, want %+v", store.startParams, want)
	}
}

func TestPaymentsStartGeneratorError(t *testing.T) {
	t.Parallel()

	store := &stubPaymentStore{}
	payments := NewPayments(store, func() (string, error) {
		return "", errors.New("no entropy")
	}, time.Minute)

	if _, err := payments.Start(context.Background(), StartPaymentRequest{}); err == nil {
		t.Fatal("expected an error")
	}
	if store.startParams != (StartPaymentParams{}) {
		t.Error("store was called after the id generator failed")
	}
}

func TestPaymentsGetForHold(t *testing.T) {
	t.Parallel()

	store := &stubPaymentStore{
		getResult: payment.Attempt{ID: "attempt-1"},
		getFound:  true,
	}
	payments := NewPayments(store, func() (string, error) { return "", nil }, time.Minute)

	attempt, found, err := payments.GetForHold(context.Background(), "hold-1", "user-1")
	if err != nil {
		t.Fatalf("GetForHold: %v", err)
	}
	if !found || attempt.ID != "attempt-1" {
		t.Errorf("found = %t, attempt = %+v", found, attempt)
	}
	if store.gotHoldID != "hold-1" || store.gotUserID != "user-1" {
		t.Errorf("get called with %q %q", store.gotHoldID, store.gotUserID)
	}
}
