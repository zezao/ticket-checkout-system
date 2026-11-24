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

// RateLimiter reports whether a user may make another hold request.
type RateLimiter interface {
	Allow(ctx context.Context, userID string) (bool, error)
}

// Metrics records HTTP-layer observations.
type Metrics interface {
	HoldAttempt(result string, duration time.Duration)
	Payment(result string)
	ConsistencyError()
}

// Catalog serves the read endpoints.
type Catalog interface {
	ListEvents(ctx context.Context) ([]app.Event, error)
	GetEvent(ctx context.Context, id string) (app.Event, error)
	SeatMap(ctx context.Context, eventID string) (app.SeatMap, error)
}

// Deps carries the adapters the HTTP API needs.
type Deps struct {
	Postgres       Pinger
	Redis          Pinger
	Catalog        Catalog
	Reservations   Reservations
	Payments       Payments
	RateLimiter    RateLimiter
	Metrics        Metrics
	MetricsHandler http.Handler
	Logger         *slog.Logger
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
		rateLimiter:  deps.RateLimiter,
		metrics:      deps.Metrics,
		logger:       deps.Logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(deps.Postgres, deps.Redis))
	mux.HandleFunc("GET /events", h.listEvents)
	mux.HandleFunc("GET /events/{id}", h.getEvent)
	mux.HandleFunc("GET /events/{id}/seats", h.listSeats)
	mux.HandleFunc("POST /events/{id}/holds", h.createHold)
	mux.HandleFunc("GET /holds/{id}", h.getHold)
	mux.HandleFunc("DELETE /holds/{id}", h.releaseHold)
	mux.HandleFunc("POST /holds/{id}/payment", h.startPayment)
	if deps.MetricsHandler != nil {
		mux.Handle("GET /metrics", deps.MetricsHandler)
	}
	mux.HandleFunc("/", handleNotFound)

	return withRequestID(withLogging(deps.Logger, withRecovery(deps.Logger, mux)))
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type readyzResponse struct {
	Status   string `json:"status"`
	Postgres string `json:"postgres"`
	Redis    string `json:"redis"`
}

func handleReadyz(postgres, redis Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		postgresStatus := "ok"
		if postgres == nil || postgres.Ping(ctx) != nil {
			postgresStatus = "unavailable"
			writeJSON(w, http.StatusServiceUnavailable, readyzResponse{
				Status:   "unavailable",
				Postgres: postgresStatus,
				Redis:    redisStatus(ctx, redis),
			})
			return
		}

		writeJSON(w, http.StatusOK, readyzResponse{
			Status:   "ok",
			Postgres: postgresStatus,
			Redis:    redisStatus(ctx, redis),
		})
	}
}

func redisStatus(ctx context.Context, redis Pinger) string {
	if redis == nil {
		return "disabled"
	}
	if err := redis.Ping(ctx); err != nil {
		return "unavailable"
	}
	return "ok"
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
