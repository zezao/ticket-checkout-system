package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type holdBody struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	SeatID    string `json:"seat_id"`
	ExpiresAt string `json:"expires_at"`
}

type paymentBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type holdEnvelopeBody struct {
	Hold    holdBody     `json:"hold"`
	Payment *paymentBody `json:"payment"`
}

func TestHoldLifecycle(t *testing.T) {
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

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userA,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire status = %d, want %d: %s", status, http.StatusCreated, data)
	}

	created := decodeHold(t, data)
	holdID := created.Hold.ID
	if _, err := uuid.Parse(holdID); err != nil {
		t.Errorf("hold id %q is not a uuid: %v", holdID, err)
	}
	if created.Hold.Status != "active" {
		t.Errorf("status = %q, want active", created.Hold.Status)
	}
	if created.Hold.SeatID != seatID {
		t.Errorf("seat id = %q, want %q", created.Hold.SeatID, seatID)
	}
	if created.Payment != nil {
		t.Errorf("payment = %+v, want nil", created.Payment)
	}
	expiresAt, err := time.Parse(time.RFC3339, created.Hold.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want a future time", expiresAt)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userA,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("duplicate acquire status = %d, want %d: %s", status, http.StatusOK, data)
	}
	if got := decodeHold(t, data).Hold.ID; got != holdID {
		t.Errorf("duplicate hold id = %q, want %q", got, holdID)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userB,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("other user status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if got := decodeError(t, data).Error.Code; got != "seat_unavailable" {
		t.Errorf("error code = %q, want seat_unavailable", got)
	}

	status, data = doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, userA, "")
	if status != http.StatusOK {
		t.Fatalf("get hold status = %d, want %d: %s", status, http.StatusOK, data)
	}
	if got := decodeHold(t, data).Hold.Status; got != "active" {
		t.Errorf("status = %q, want active", got)
	}

	status, _ = doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, userB, "")
	if status != http.StatusNotFound {
		t.Errorf("foreign get status = %d, want %d", status, http.StatusNotFound)
	}

	status, _ = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, userB, "")
	if status != http.StatusNotFound {
		t.Errorf("foreign delete status = %d, want %d", status, http.StatusNotFound)
	}

	status, data = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, userA, "")
	if status != http.StatusNoContent {
		t.Fatalf("release status = %d, want %d: %s", status, http.StatusNoContent, data)
	}

	status, data = doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, userA, "")
	if status != http.StatusOK {
		t.Fatalf("get released hold status = %d: %s", status, data)
	}
	if got := decodeHold(t, data).Hold.Status; got != "released" {
		t.Errorf("status = %q, want released", got)
	}

	status, data = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, userA, "")
	if status != http.StatusConflict {
		t.Fatalf("second release status = %d, want %d", status, http.StatusConflict)
	}
	if got := decodeError(t, data).Error.Code; got != "hold_not_active" {
		t.Errorf("error code = %q, want hold_not_active", got)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userB,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire after release status = %d: %s", status, data)
	}
	if got := decodeHold(t, data).Hold.ID; got == holdID {
		t.Error("released hold id was reused")
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userA,
		`{"seat_id":"`+uuid.NewString()+`"}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown seat status = %d, want %d: %s", status, http.StatusNotFound, data)
	}

	var outboxRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		 WHERE topic = 'seat.changed'
		   AND payload->>'seat_id' = $1`, seatID,
	).Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxRows != 3 {
		t.Errorf("outbox rows = %d, want 3", outboxRows)
	}

	checkInvariants(t, pool)
}

func TestInvariant03_OnlyOwnerCanRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)

	owner := uuid.NewString()
	other := uuid.NewString()

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", owner,
		`{"seat_id":"`+seatIDs[0]+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire status = %d: %s", status, data)
	}
	holdID := decodeHold(t, data).Hold.ID

	if status, _ = doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, other, ""); status != http.StatusNotFound {
		t.Errorf("foreign get status = %d, want %d", status, http.StatusNotFound)
	}
	if status, _ = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, other, ""); status != http.StatusNotFound {
		t.Errorf("foreign delete status = %d, want %d", status, http.StatusNotFound)
	}

	status, data = doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, owner, "")
	if status != http.StatusOK {
		t.Fatalf("owner get status = %d: %s", status, data)
	}
	if got := decodeHold(t, data).Hold.Status; got != "active" {
		t.Errorf("hold status = %q, want active", got)
	}

	if status, data = doRequest(t, http.MethodDelete, server.URL+"/holds/"+holdID, owner, ""); status != http.StatusNoContent {
		t.Fatalf("owner delete status = %d: %s", status, data)
	}

	checkInvariants(t, pool)
}

func doRequest(t *testing.T, method, url, userID, body string) (int, []byte) {
	t.Helper()

	status, data, err := rawRequest(context.Background(), method, url, userID, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return status, data
}

func rawRequest(ctx context.Context, method, url, userID, body string) (int, []byte, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func decodeHold(t *testing.T, data []byte) holdEnvelopeBody {
	t.Helper()

	var body holdEnvelopeBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode hold: %v (body %s)", err, data)
	}
	return body
}

func decodeError(t *testing.T, data []byte) errorDTO {
	t.Helper()

	var body errorDTO
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode error: %v (body %s)", err, data)
	}
	return body
}

func insertHold(
	t *testing.T,
	pool *pgxpool.Pool,
	holdID, seatID, userID string,
	status reservation.HoldStatus,
	expiresAt time.Time,
) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO holds (id, seat_id, user_id, status, expires_at, amount_cents, currency)
		VALUES ($1::uuid, $2::uuid, $3::uuid, ($4::text)::hold_status, $5, 8500, 'USD')`,
		holdID, seatID, userID, string(status), expiresAt,
	); err != nil {
		t.Fatalf("insert hold: %v", err)
	}
}

func holdSeat(t *testing.T, pool *pgxpool.Pool, seatID, holdID string, expiresAt *time.Time) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), `
		UPDATE seats
		   SET status = 'held', current_hold_id = $1::uuid, hold_expires_at = $2
		 WHERE id = $3::uuid`,
		holdID, expiresAt, seatID,
	); err != nil {
		t.Fatalf("hold seat: %v", err)
	}
}
