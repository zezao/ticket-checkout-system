package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// SeatMapMetrics records cache invalidation failures.
type SeatMapMetrics interface {
	RedisError()
}

// SeatChangedHandler invalidates the cached seat map for one event.
type SeatChangedHandler struct {
	cache   app.SeatMapCache
	logger  *slog.Logger
	metrics SeatMapMetrics
}

// NewSeatChangedHandler builds the seat.changed consumer.
func NewSeatChangedHandler(
	cache app.SeatMapCache,
	logger *slog.Logger,
	metrics SeatMapMetrics,
) *SeatChangedHandler {
	return &SeatChangedHandler{cache: cache, logger: logger, metrics: metrics}
}

// Handle deletes the event's seat-map cache key. Deletion is idempotent.
func (h *SeatChangedHandler) Handle(ctx context.Context, message app.OutboxMessage) error {
	var payload struct {
		EventID string `json:"event_id"`
		SeatID  string `json:"seat_id"`
	}
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		return fmt.Errorf("decode seat.changed payload: %w", err)
	}
	if payload.EventID == "" {
		return fmt.Errorf("seat.changed payload is missing event_id")
	}

	if err := h.cache.Delete(ctx, payload.EventID); err != nil {
		if h.metrics != nil {
			h.metrics.RedisError()
		}
		return fmt.Errorf("invalidate seat map: %w", err)
	}
	if h.logger != nil {
		h.logger.DebugContext(ctx, "invalidated seat map cache",
			"event_id", payload.EventID,
			"seat_id", payload.SeatID,
		)
	}
	return nil
}
