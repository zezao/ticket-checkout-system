package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type handlers struct {
	catalog      Catalog
	reservations Reservations
	payments     Payments
	rateLimiter  RateLimiter
	metrics      Metrics
	logger       *slog.Logger
}

type eventResponse struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Venue    string    `json:"venue"`
	StartsAt time.Time `json:"starts_at"`
	Currency string    `json:"currency"`
}

type eventsResponse struct {
	Events []eventResponse `json:"events"`
}

type seatResponse struct {
	ID         string `json:"id"`
	Section    string `json:"section"`
	Row        string `json:"row"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency"`
}

type seatsResponse struct {
	Seats []seatResponse `json:"seats"`
}

func (h *handlers) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := h.catalog.ListEvents(r.Context())
	if err != nil {
		h.internalError(w, r, "list events", err)
		return
	}

	response := eventsResponse{Events: make([]eventResponse, 0, len(events))}
	for _, event := range events {
		response.Events = append(response.Events, newEventResponse(event))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *handlers) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := eventIDFromPath(w, r)
	if !ok {
		return
	}

	event, err := h.catalog.GetEvent(r.Context(), id)
	if errors.Is(err, app.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	if err != nil {
		h.internalError(w, r, "get event", err)
		return
	}

	writeJSON(w, http.StatusOK, newEventResponse(event))
}

func (h *handlers) listSeats(w http.ResponseWriter, r *http.Request) {
	id, ok := eventIDFromPath(w, r)
	if !ok {
		return
	}

	seatMap, err := h.catalog.SeatMap(r.Context(), id)
	if errors.Is(err, app.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	if err != nil {
		h.internalError(w, r, "list seats", err)
		return
	}

	response := seatsResponse{Seats: make([]seatResponse, 0, len(seatMap.Seats))}
	for _, seat := range seatMap.Seats {
		response.Seats = append(response.Seats, newSeatResponse(seat, seatMap.Event.Currency))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *handlers) internalError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	if h.metrics != nil && errors.Is(err, app.ErrConsistency) {
		h.metrics.ConsistencyError()
	}
	h.logger.ErrorContext(r.Context(), "request failed",
		"operation", operation,
		"error", err,
		"request_id", RequestID(r.Context()),
	)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func eventIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "event not found")
		return "", false
	}
	return id.String(), true
}

func newEventResponse(event app.Event) eventResponse {
	return eventResponse{
		ID:       event.ID,
		Name:     event.Name,
		Venue:    event.Venue,
		StartsAt: event.StartsAt.UTC(),
		Currency: event.Currency,
	}
}

func newSeatResponse(seat reservation.Seat, currency string) seatResponse {
	return seatResponse{
		ID:         seat.ID,
		Section:    seat.Section,
		Row:        seat.RowLabel,
		Number:     seat.Number,
		Status:     string(seat.Status),
		PriceCents: seat.PriceCents,
		Currency:   currency,
	}
}
