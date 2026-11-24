package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	maxIdempotencyKeyLen = 255
)

// Payments serves the payment endpoints and the poll view.
type Payments interface {
	Start(ctx context.Context, req app.StartPaymentRequest) (payment.Attempt, error)
	GetForHold(ctx context.Context, holdID, userID string) (payment.Attempt, bool, error)
}

func (h *handlers) startPayment(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromRequest(w, r)
	if !ok {
		return
	}
	holdID, ok := holdIDFromPath(w, r)
	if !ok {
		return
	}

	key := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if key == "" || len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, "idempotency_key_required",
			"a valid Idempotency-Key header is required")
		return
	}

	attempt, err := h.payments.Start(r.Context(), app.StartPaymentRequest{
		HoldID:         holdID,
		UserID:         userID,
		IdempotencyKey: key,
		Traceparent:    r.Header.Get("traceparent"),
	})
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "hold not found")
		return
	case errors.Is(err, app.ErrHoldNotActive):
		writeError(w, http.StatusConflict, "hold_not_active", "hold is not active")
		return
	case errors.Is(err, app.ErrPaymentInProgress):
		writeError(w, http.StatusConflict, "payment_in_progress", "a payment is already in progress")
		return
	case errors.Is(err, app.ErrIdempotencyKeyReused):
		writeError(w, http.StatusConflict, "idempotency_key_reused",
			"idempotency key was used on another hold")
		return
	case err != nil:
		h.internalError(w, r, "start payment", err)
		return
	}

	hold, err := h.reservations.Get(r.Context(), holdID, userID)
	if err != nil {
		h.internalError(w, r, "read hold after payment start", err)
		return
	}

	status := http.StatusAccepted
	if !attempt.Open() {
		status = http.StatusOK
	}

	envelope := newHoldEnvelope(hold)
	envelope.Payment = newPaymentResponse(attempt)
	writeJSON(w, status, envelope)
}

func (h *handlers) paymentForHold(
	ctx context.Context,
	holdID, userID string,
) (*paymentResponse, error) {
	attempt, found, err := h.payments.GetForHold(ctx, holdID, userID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return newPaymentResponse(attempt), nil
}

func newPaymentResponse(attempt payment.Attempt) *paymentResponse {
	return &paymentResponse{
		ID:     attempt.ID,
		Status: string(attempt.Status),
	}
}
