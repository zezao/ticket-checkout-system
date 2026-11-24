package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const testAttemptID = "0199a000-0000-7000-8000-000000000802"

type fakePayments struct {
	startResult payment.Attempt
	startErr    error
	startCalls  int
	startReq    app.StartPaymentRequest
	getResult   payment.Attempt
	getFound    bool
	getErr      error
	getCalls    int
}

func (f *fakePayments) Start(_ context.Context, req app.StartPaymentRequest) (payment.Attempt, error) {
	f.startCalls++
	f.startReq = req
	return f.startResult, f.startErr
}

func (f *fakePayments) GetForHold(context.Context, string, string) (payment.Attempt, bool, error) {
	f.getCalls++
	return f.getResult, f.getFound, f.getErr
}

func TestStartPaymentAccepted(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{hold: reservation.Hold{
		ID:        testHoldID,
		SeatID:    testSeatID,
		Status:    reservation.HoldPaymentPending,
		ExpiresAt: time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC),
	}}
	payments := &fakePayments{startResult: payment.Attempt{
		ID:     testAttemptID,
		Status: payment.StatusCreated,
	}}

	rec := serveHoldWithPayments(reservations, payments, http.MethodPost,
		"/holds/"+testHoldID+"/payment", "",
		map[string]string{
			userIDHeader:         testUserID,
			idempotencyKeyHeader: "key-1",
			"traceparent":        "00-trace-01",
		})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}

	var body holdEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Hold.Status != "payment_pending" {
		t.Errorf("hold status = %q, want payment_pending", body.Hold.Status)
	}
	if body.Payment == nil {
		t.Fatal("payment is nil")
	}
	if body.Payment.ID != testAttemptID || body.Payment.Status != "created" {
		t.Errorf("payment = %+v", body.Payment)
	}

	wantReq := app.StartPaymentRequest{
		HoldID:         testHoldID,
		UserID:         testUserID,
		IdempotencyKey: "key-1",
		Traceparent:    "00-trace-01",
	}
	if payments.startReq != wantReq {
		t.Errorf("start request = %+v, want %+v", payments.startReq, wantReq)
	}
}

func TestStartPaymentFinalReturnsOK(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{hold: reservation.Hold{
		ID:        testHoldID,
		Status:    reservation.HoldConfirmed,
		ExpiresAt: time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC),
	}}
	payments := &fakePayments{startResult: payment.Attempt{
		ID:     testAttemptID,
		Status: payment.StatusSucceeded,
	}}

	rec := serveHoldWithPayments(reservations, payments, http.MethodPost,
		"/holds/"+testHoldID+"/payment", "",
		map[string]string{
			userIDHeader:         testUserID,
			idempotencyKeyHeader: "key-1",
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestStartPaymentRequiresKey(t *testing.T) {
	t.Parallel()

	payments := &fakePayments{}
	rec := serveHoldWithPayments(&fakeReservations{}, payments, http.MethodPost,
		"/holds/"+testHoldID+"/payment", "",
		map[string]string{userIDHeader: testUserID})

	assertError(t, rec, http.StatusBadRequest, "idempotency_key_required")
	if payments.startCalls != 0 {
		t.Errorf("start calls = %d, want 0", payments.startCalls)
	}
}

func TestStartPaymentRejectsLongKey(t *testing.T) {
	t.Parallel()

	payments := &fakePayments{}
	rec := serveHoldWithPayments(&fakeReservations{}, payments, http.MethodPost,
		"/holds/"+testHoldID+"/payment", "",
		map[string]string{
			userIDHeader:         testUserID,
			idempotencyKeyHeader: strings.Repeat("x", maxIdempotencyKeyLen+1),
		})

	assertError(t, rec, http.StatusBadRequest, "idempotency_key_required")
}

func TestStartPaymentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "not found", err: app.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "hold not active", err: app.ErrHoldNotActive, wantStatus: http.StatusConflict, wantCode: "hold_not_active"},
		{name: "payment in progress", err: app.ErrPaymentInProgress, wantStatus: http.StatusConflict, wantCode: "payment_in_progress"},
		{name: "key reused", err: app.ErrIdempotencyKeyReused, wantStatus: http.StatusConflict, wantCode: "idempotency_key_reused"},
		{name: "internal", err: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payments := &fakePayments{startErr: tt.err}
			rec := serveHoldWithPayments(&fakeReservations{}, payments, http.MethodPost,
				"/holds/"+testHoldID+"/payment", "",
				map[string]string{
					userIDHeader:         testUserID,
					idempotencyKeyHeader: "key-1",
				})

			assertError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestStartPaymentRequiresUserID(t *testing.T) {
	t.Parallel()

	payments := &fakePayments{}
	rec := serveHoldWithPayments(&fakeReservations{}, payments, http.MethodPost,
		"/holds/"+testHoldID+"/payment", "",
		map[string]string{idempotencyKeyHeader: "key-1"})

	assertError(t, rec, http.StatusUnauthorized, "user_id_required")
	if payments.startCalls != 0 {
		t.Errorf("start calls = %d, want 0", payments.startCalls)
	}
}

func TestStartPaymentMalformedHoldID(t *testing.T) {
	t.Parallel()

	payments := &fakePayments{}
	rec := serveHoldWithPayments(&fakeReservations{}, payments, http.MethodPost,
		"/holds/not-a-uuid/payment", "",
		map[string]string{
			userIDHeader:         testUserID,
			idempotencyKeyHeader: "key-1",
		})

	assertError(t, rec, http.StatusNotFound, "not_found")
	if payments.startCalls != 0 {
		t.Errorf("start calls = %d, want 0", payments.startCalls)
	}
}

func TestGetHoldIncludesPayment(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{hold: reservation.Hold{
		ID:        testHoldID,
		Status:    reservation.HoldPaymentPending,
		ExpiresAt: time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC),
	}}
	payments := &fakePayments{
		getResult: payment.Attempt{ID: testAttemptID, Status: payment.StatusPending},
		getFound:  true,
	}

	rec := serveHoldWithPayments(reservations, payments, http.MethodGet,
		"/holds/"+testHoldID, "", map[string]string{userIDHeader: testUserID})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body holdEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Payment == nil {
		t.Fatal("payment is nil")
	}
	if body.Payment.ID != testAttemptID || body.Payment.Status != "pending" {
		t.Errorf("payment = %+v", body.Payment)
	}
}

func TestCreateHoldIncludesPaymentWhenPending(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{acquireResult: app.AcquireResult{
		Hold: reservation.Hold{
			ID:        testHoldID,
			SeatID:    testSeatID,
			Status:    reservation.HoldPaymentPending,
			ExpiresAt: time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC),
		},
		Created: false,
	}}
	payments := &fakePayments{
		getResult: payment.Attempt{ID: testAttemptID, Status: payment.StatusCreated},
		getFound:  true,
	}

	rec := serveHoldWithPayments(reservations, payments, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body holdEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Payment == nil {
		t.Fatal("payment is nil")
	}
	if body.Payment.ID != testAttemptID {
		t.Errorf("payment id = %q, want %q", body.Payment.ID, testAttemptID)
	}
}
