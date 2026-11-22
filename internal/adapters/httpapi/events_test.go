package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const testEventID = "0199a000-0000-7000-8000-000000000001"

type fakeCatalog struct {
	events    []app.Event
	listErr   error
	event     app.Event
	eventErr  error
	seatMap   app.SeatMap
	seatErr   error
	getCalls  int
	seatCalls int
}

func (f *fakeCatalog) ListEvents(context.Context) ([]app.Event, error) {
	return f.events, f.listErr
}

func (f *fakeCatalog) GetEvent(context.Context, string) (app.Event, error) {
	f.getCalls++
	return f.event, f.eventErr
}

func (f *fakeCatalog) SeatMap(context.Context, string) (app.SeatMap, error) {
	f.seatCalls++
	return f.seatMap, f.seatErr
}

func serve(catalog Catalog, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	NewHandler(Deps{
		Postgres: stubPinger{},
		Catalog:  catalog,
		Logger:   testLogger(),
	}).ServeHTTP(rec, req)
	return rec
}

func TestListEvents(t *testing.T) {
	t.Parallel()

	catalog := &fakeCatalog{events: []app.Event{{
		ID:       testEventID,
		Name:     "Go Concurrency Live",
		Venue:    "The Sandbox",
		StartsAt: time.Date(2027, 6, 12, 19, 30, 0, 0, time.UTC),
		Currency: "USD",
	}}}

	rec := serve(catalog, http.MethodGet, "/events")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Events []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			StartsAt string `json:"starts_at"`
			Currency string `json:"currency"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(body.Events))
	}
	event := body.Events[0]
	if event.ID != testEventID {
		t.Errorf("id = %q, want %q", event.ID, testEventID)
	}
	if event.StartsAt != "2027-06-12T19:30:00Z" {
		t.Errorf("starts_at = %q, want %q", event.StartsAt, "2027-06-12T19:30:00Z")
	}
	if event.Currency != "USD" {
		t.Errorf("currency = %q, want %q", event.Currency, "USD")
	}
}

func TestListEventsEmpty(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{}, http.MethodGet, "/events")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Errorf("body = %q, want an empty events array", rec.Body.String())
	}
}

func TestListEventsStoreError(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{listErr: errors.New("database unavailable")}, http.MethodGet, "/events")

	assertError(t, rec, http.StatusInternalServerError, "internal_error")
}

func TestGetEvent(t *testing.T) {
	t.Parallel()

	catalog := &fakeCatalog{event: app.Event{
		ID:       testEventID,
		Name:     "Go Concurrency Live",
		Venue:    "The Sandbox",
		StartsAt: time.Date(2027, 6, 12, 19, 30, 0, 0, time.UTC),
		Currency: "USD",
	}}

	rec := serve(catalog, http.MethodGet, "/events/"+testEventID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != testEventID {
		t.Errorf("id = %q, want %q", body.ID, testEventID)
	}
}

func TestGetEventNotFound(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{eventErr: app.ErrNotFound}, http.MethodGet, "/events/"+testEventID)

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestGetEventInvalidID(t *testing.T) {
	t.Parallel()

	catalog := &fakeCatalog{}
	rec := serve(catalog, http.MethodGet, "/events/not-a-uuid")

	assertError(t, rec, http.StatusNotFound, "not_found")
	if catalog.getCalls != 0 {
		t.Errorf("store calls = %d, want 0", catalog.getCalls)
	}
}

func TestGetEventStoreError(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{eventErr: errors.New("database unavailable")}, http.MethodGet, "/events/"+testEventID)

	assertError(t, rec, http.StatusInternalServerError, "internal_error")
}

func TestListSeats(t *testing.T) {
	t.Parallel()

	catalog := &fakeCatalog{seatMap: app.SeatMap{
		Event: app.Event{ID: testEventID, Currency: "USD"},
		Seats: []reservation.Seat{{
			ID:         "0199a000-0000-7000-8000-000000000101",
			EventID:    testEventID,
			Section:    "Orchestra",
			RowLabel:   "A",
			Number:     1,
			PriceCents: 8500,
			Status:     reservation.SeatAvailable,
		}},
	}}

	rec := serve(catalog, http.MethodGet, "/events/"+testEventID+"/seats")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Seats []struct {
			ID         string `json:"id"`
			Section    string `json:"section"`
			Row        string `json:"row"`
			Number     int    `json:"number"`
			Status     string `json:"status"`
			PriceCents int64  `json:"price_cents"`
			Currency   string `json:"currency"`
		} `json:"seats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Seats) != 1 {
		t.Fatalf("got %d seats, want 1", len(body.Seats))
	}
	seat := body.Seats[0]
	if seat.Row != "A" || seat.Number != 1 {
		t.Errorf("seat = row %q number %d", seat.Row, seat.Number)
	}
	if seat.Status != "available" {
		t.Errorf("status = %q, want %q", seat.Status, "available")
	}
	if seat.PriceCents != 8500 || seat.Currency != "USD" {
		t.Errorf("price = %d %s", seat.PriceCents, seat.Currency)
	}
}

func TestListSeatsNotFound(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{seatErr: app.ErrNotFound}, http.MethodGet, "/events/"+testEventID+"/seats")

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestListSeatsStoreError(t *testing.T) {
	t.Parallel()

	rec := serve(&fakeCatalog{seatErr: errors.New("database unavailable")}, http.MethodGet, "/events/"+testEventID+"/seats")

	assertError(t, rec, http.StatusInternalServerError, "internal_error")
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if got := rec.Header().Get("X-Request-ID"); got == "" {
		t.Error("X-Request-ID header is empty")
	}

	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != code {
		t.Errorf("error code = %q, want %q", body.Error.Code, code)
	}
	if body.Error.Message == "" {
		t.Error("error message is empty")
	}
}
