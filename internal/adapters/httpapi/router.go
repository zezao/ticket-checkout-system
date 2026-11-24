// Package httpapi exposes the HTTP API.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

const readinessTimeout = 2 * time.Second

// Pinger reports whether a dependency is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Catalog serves the read endpoints.
type Catalog interface {
	ListEvents(ctx context.Context) ([]app.Event, error)
	GetEvent(ctx context.Context, id string) (app.Event, error)
	SeatMap(ctx context.Context, eventID string) (app.SeatMap, error)
}

// Deps carries the adapters the HTTP API needs.
type Deps struct {
	Postgres     Pinger
	Catalog      Catalog
	Reservations Reservations
	Payments     Payments
	Logger       *slog.Logger
}

// NewHandler builds the API router.
func NewHandler(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	h := &handlers{
		catalog:      deps.Catalog,
		reservations: deps.Reservations,
		payments:     deps.Payments,
		logger:       deps.Logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(deps.Postgres))
	mux.HandleFunc("GET /events", h.listEvents)
	mux.HandleFunc("GET /events/{id}", h.getEvent)
	mux.HandleFunc("GET /events/{id}/seats", h.listSeats)
	mux.HandleFunc("POST /events/{id}/holds", h.createHold)
	mux.HandleFunc("GET /holds/{id}", h.getHold)
	mux.HandleFunc("DELETE /holds/{id}", h.releaseHold)
	mux.HandleFunc("POST /holds/{id}/payment", h.startPayment)
	mux.HandleFunc("/", handleNotFound)

	return withRequestID(withLogging(deps.Logger, withRecovery(deps.Logger, mux)))
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleReadyz(postgres Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		if err := postgres.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
