package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const (
	testSeatID = "0199a000-0000-7000-8000-000000000101"
	testUserID = "0199a000-0000-7000-8000-000000000901"
	testHoldID = "0199a000-0000-7000-8000-000000000801"
)

type fakeReservations struct {
	acquireResult  app.AcquireResult
	acquireErr     error
	acquireCalls   int
	acquireReq     app.AcquireRequest
	releaseErr     error
	releasedHoldID string
	releasedUserID string
	hold           reservation.Hold
	getErr         error
	getCalls       int
	gotHoldID      string
	gotUserID      string
}

func (f *fakeReservations) Acquire(_ context.Context, req app.AcquireRequest) (app.AcquireResult, error) {
	f.acquireCalls++
	f.acquireReq = req
	return f.acquireResult, f.acquireErr
}

func (f *fakeReservations) Release(_ context.Context, holdID, userID string) error {
	f.releasedHoldID = holdID
	f.releasedUserID = userID
	return f.releaseErr
}

func (f *fakeReservations) Get(_ context.Context, holdID, userID string) (reservation.Hold, error) {
	f.getCalls++
	f.gotHoldID = holdID
	f.gotUserID = userID
	return f.hold, f.getErr
}

func serveHold(
	reservations Reservations,
	method, path, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	NewHandler(Deps{
		Postgres:     stubPinger{},
		Reservations: reservations,
		Logger:       testLogger(),
	}).ServeHTTP(rec, req)

	return rec
}

func TestCreateHoldCreated(t *testing.T) {
	t.Parallel()

	expiresAt := time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC)
	reservations := &fakeReservations{acquireResult: app.AcquireResult{
		Hold: reservation.Hold{
			ID:        testHoldID,
			SeatID:    testSeatID,
			Status:    reservation.HoldActive,
			ExpiresAt: expiresAt,
		},
		Created: true,
	}}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID},
	)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body)
	}

	var body holdEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Hold.ID != testHoldID {
		t.Errorf("hold id = %q, want %q", body.Hold.ID, testHoldID)
	}
	if body.Hold.Status != "active" {
		t.Errorf("status = %q, want %q", body.Hold.Status, "active")
	}
	if !body.Hold.ExpiresAt.Equal(expiresAt) {
		t.Errorf("expires_at = %v, want %v", body.Hold.ExpiresAt, expiresAt)
	}
	if body.Payment != nil {
		t.Errorf("payment = %+v, want nil", body.Payment)
	}

	if reservations.acquireReq.EventID != testEventID ||
		reservations.acquireReq.SeatID != testSeatID ||
		reservations.acquireReq.UserID != testUserID {
		t.Errorf("acquire request = %+v", reservations.acquireReq)
	}
}

func TestCreateHoldExisting(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{acquireResult: app.AcquireResult{
		Hold: reservation.Hold{
			ID:        testHoldID,
			SeatID:    testSeatID,
			Status:    reservation.HoldActive,
			ExpiresAt: time.Now().Add(time.Minute),
		},
		Created: false,
	}}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID},
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestCreateHoldSeatUnavailable(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{acquireErr: app.ErrSeatUnavailable}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusConflict, "seat_unavailable")
}

func TestCreateHoldSeatNotFound(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{acquireErr: app.ErrNotFound}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestCreateHoldMalformedSeatID(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"not-a-uuid"}`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusNotFound, "not_found")
	if reservations.acquireCalls != 0 {
		t.Errorf("acquire calls = %d, want 0", reservations.acquireCalls)
	}
}

func TestCreateHoldMissingSeatID(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{}, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{}`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestCreateHoldInvalidJSON(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{}, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestCreateHoldRequiresUserID(t *testing.T) {
	t.Parallel()

	for _, header := range []string{"", "not-a-uuid"} {
		reservations := &fakeReservations{}
		headers := map[string]string{}
		if header != "" {
			headers[userIDHeader] = header
		}

		rec := serveHold(reservations, http.MethodPost,
			"/events/"+testEventID+"/holds",
			`{"seat_id":"`+testSeatID+`"}`,
			headers,
		)

		assertError(t, rec, http.StatusUnauthorized, "user_id_required")
		if reservations.acquireCalls != 0 {
			t.Errorf("acquire calls = %d, want 0", reservations.acquireCalls)
		}
	}
}

func TestCreateHoldStoreError(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{acquireErr: errors.New("database unavailable")}

	rec := serveHold(reservations, http.MethodPost,
		"/events/"+testEventID+"/holds",
		`{"seat_id":"`+testSeatID+`"}`,
		map[string]string{userIDHeader: testUserID},
	)

	assertError(t, rec, http.StatusInternalServerError, "internal_error")
}

func TestGetHold(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{hold: reservation.Hold{
		ID:        testHoldID,
		SeatID:    testSeatID,
		Status:    reservation.HoldPaymentPending,
		ExpiresAt: time.Date(2027, 6, 12, 19, 40, 0, 0, time.UTC),
	}}

	rec := serveHold(reservations, http.MethodGet, "/holds/"+testHoldID, "",
		map[string]string{userIDHeader: testUserID})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body holdEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Hold.Status != "payment_pending" {
		t.Errorf("status = %q", body.Hold.Status)
	}
	if body.Payment != nil {
		t.Errorf("payment = %+v, want nil", body.Payment)
	}
	if reservations.gotHoldID != testHoldID || reservations.gotUserID != testUserID {
		t.Errorf("get called with %q %q", reservations.gotHoldID, reservations.gotUserID)
	}
}

func TestGetHoldNotFound(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{getErr: app.ErrNotFound},
		http.MethodGet, "/holds/"+testHoldID, "",
		map[string]string{userIDHeader: testUserID})

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestGetHoldMalformedID(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{}
	rec := serveHold(reservations, http.MethodGet, "/holds/not-a-uuid", "",
		map[string]string{userIDHeader: testUserID})

	assertError(t, rec, http.StatusNotFound, "not_found")
	if reservations.getCalls != 0 {
		t.Errorf("get calls = %d, want 0", reservations.getCalls)
	}
}

func TestReleaseHold(t *testing.T) {
	t.Parallel()

	reservations := &fakeReservations{}

	rec := serveHold(reservations, http.MethodDelete, "/holds/"+testHoldID, "",
		map[string]string{userIDHeader: testUserID})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if reservations.releasedHoldID != testHoldID || reservations.releasedUserID != testUserID {
		t.Errorf("release called with %q %q", reservations.releasedHoldID, reservations.releasedUserID)
	}
}

func TestReleaseHoldNotFound(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{releaseErr: app.ErrNotFound},
		http.MethodDelete, "/holds/"+testHoldID, "",
		map[string]string{userIDHeader: testUserID})

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestReleaseHoldNotActive(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{releaseErr: app.ErrHoldNotActive},
		http.MethodDelete, "/holds/"+testHoldID, "",
		map[string]string{userIDHeader: testUserID})

	assertError(t, rec, http.StatusConflict, "hold_not_active")
}

func TestReleaseHoldRequiresUserID(t *testing.T) {
	t.Parallel()

	rec := serveHold(&fakeReservations{}, http.MethodDelete, "/holds/"+testHoldID, "", nil)

	assertError(t, rec, http.StatusUnauthorized, "user_id_required")
}
