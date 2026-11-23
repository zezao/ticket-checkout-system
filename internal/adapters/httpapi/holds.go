package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const (
	userIDHeader = "X-User-ID"
	maxBodyBytes = 1 << 20
)

// Reservations serves the hold lifecycle endpoints.
type Reservations interface {
	Acquire(ctx context.Context, req app.AcquireRequest) (app.AcquireResult, error)
	Release(ctx context.Context, holdID, userID string) error
	Get(ctx context.Context, holdID, userID string) (reservation.Hold, error)
}

type holdResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	SeatID    string    `json:"seat_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type paymentResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type holdEnvelope struct {
	Hold    holdResponse     `json:"hold"`
	Payment *paymentResponse `json:"payment"`
}

type createHoldRequest struct {
	SeatID string `json:"seat_id"`
}

func (h *handlers) createHold(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromRequest(w, r)
	if !ok {
		return
	}
	eventID, ok := eventIDFromPath(w, r)
	if !ok {
		return
	}

	var body createHoldRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if body.SeatID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "seat_id is required")
		return
	}

	seatID, err := uuid.Parse(body.SeatID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "seat not found")
		return
	}

	result, err := h.reservations.Acquire(r.Context(), app.AcquireRequest{
		EventID: eventID,
		SeatID:  seatID.String(),
		UserID:  userID,
	})
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "seat not found")
		return
	case errors.Is(err, app.ErrSeatUnavailable):
		writeError(w, http.StatusConflict, "seat_unavailable", "seat is not available")
		return
	case err != nil:
		h.internalError(w, r, "acquire hold", err)
		return
	}

	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, newHoldEnvelope(result.Hold))
}

func (h *handlers) getHold(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromRequest(w, r)
	if !ok {
		return
	}
	holdID, ok := holdIDFromPath(w, r)
	if !ok {
		return
	}

	hold, err := h.reservations.Get(r.Context(), holdID, userID)
	if errors.Is(err, app.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "hold not found")
		return
	}
	if err != nil {
		h.internalError(w, r, "get hold", err)
		return
	}

	writeJSON(w, http.StatusOK, newHoldEnvelope(hold))
}

func (h *handlers) releaseHold(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromRequest(w, r)
	if !ok {
		return
	}
	holdID, ok := holdIDFromPath(w, r)
	if !ok {
		return
	}

	err := h.reservations.Release(r.Context(), holdID, userID)
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "hold not found")
		return
	case errors.Is(err, app.ErrHoldNotActive):
		writeError(w, http.StatusConflict, "hold_not_active", "hold is not active")
		return
	case err != nil:
		h.internalError(w, r, "release hold", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func userIDFromRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(r.Header.Get(userIDHeader))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user_id_required", "a valid X-User-ID header is required")
		return "", false
	}
	return id.String(), true
}

func holdIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "hold not found")
		return "", false
	}
	return id.String(), true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON body")
	}
	return nil
}

func newHoldEnvelope(hold reservation.Hold) holdEnvelope {
	return holdEnvelope{
		Hold: holdResponse{
			ID:        hold.ID,
			Status:    string(hold.Status),
			SeatID:    hold.SeatID,
			ExpiresAt: hold.ExpiresAt.UTC(),
		},
	}
}
