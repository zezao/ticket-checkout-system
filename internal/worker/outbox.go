package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

const outboxBatchSize = 100

// OutboxHandler consumes one outbox message. Handlers must be idempotent.
type OutboxHandler interface {
	Handle(ctx context.Context, message app.OutboxMessage) error
}

// OutboxMetrics records relay observations.
type OutboxMetrics interface {
	OutboxLag(lag time.Duration)
	ConsistencyError()
}

// OutboxRelay publishes outbox rows with at-least-once delivery.
type OutboxRelay struct {
	store    app.OutboxStore
	handlers map[string]OutboxHandler
	logger   *slog.Logger
	lease    time.Duration
	interval time.Duration
	metrics  OutboxMetrics
}

// NewOutboxRelay builds the outbox relay loop.
func NewOutboxRelay(
	store app.OutboxStore,
	handlers map[string]OutboxHandler,
	logger *slog.Logger,
	lease, interval time.Duration,
	metrics OutboxMetrics,
) *OutboxRelay {
	return &OutboxRelay{
		store:    store,
		handlers: handlers,
		logger:   logger,
		lease:    lease,
		interval: interval,
		metrics:  metrics,
	}
}

// Run relays outbox rows until ctx is cancelled.
func (r *OutboxRelay) Run(ctx context.Context) {
	r.tick(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *OutboxRelay) tick(ctx context.Context) {
	messages, err := r.store.ClaimOutbox(ctx, r.lease, outboxBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "claim outbox failed", "error", err)
		}
		return
	}

	published := make([]int64, 0, len(messages))
	for _, message := range messages {
		if ctx.Err() != nil {
			return
		}

		handler := r.handlers[message.Topic]
		if handler == nil {
			r.logger.WarnContext(ctx, "no handler for outbox topic; dropping",
				"topic", message.Topic,
				"outbox_id", message.ID,
			)
			published = append(published, message.ID)
			continue
		}

		handleCtx, span := observability.ContinueTrace(ctx, message.Traceparent,
			"outbox."+message.Topic)
		err := handler.Handle(handleCtx, message)
		span.End()
		if err != nil {
			r.logger.ErrorContext(ctx, "outbox handler failed",
				"topic", message.Topic,
				"outbox_id", message.ID,
				"error", err,
			)
			continue
		}
		published = append(published, message.ID)
	}

	if len(published) > 0 {
		if err := r.store.MarkPublished(ctx, published); err != nil {
			if r.metrics != nil && errors.Is(err, app.ErrConsistency) {
				r.metrics.ConsistencyError()
			}
			r.logger.ErrorContext(ctx, "mark outbox published failed",
				"count", len(published),
				"error", err,
			)
		}
	}

	lag, err := r.store.OutboxLag(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "read outbox lag failed", "error", err)
		}
		return
	}
	if r.metrics != nil {
		r.metrics.OutboxLag(lag)
	}
}

// NoopHandler acknowledges messages without side effects.
type NoopHandler struct{}

// Handle does nothing.
func (NoopHandler) Handle(context.Context, app.OutboxMessage) error {
	return nil
}
