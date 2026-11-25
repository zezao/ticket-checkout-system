package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

const outboxBatchSize = 1000

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
	store        app.OutboxStore
	handlers     map[string]OutboxHandler
	logger       *slog.Logger
	lease        time.Duration
	idleInterval time.Duration
	busyInterval time.Duration
	metrics      OutboxMetrics
}

// NewOutboxRelay builds the outbox relay loop. A full claim switches the next
// tick from idleInterval to busyInterval until the backlog drops.
func NewOutboxRelay(
	store app.OutboxStore,
	handlers map[string]OutboxHandler,
	logger *slog.Logger,
	lease, idleInterval, busyInterval time.Duration,
	metrics OutboxMetrics,
) *OutboxRelay {
	return &OutboxRelay{
		store:        store,
		handlers:     handlers,
		logger:       logger,
		lease:        lease,
		idleInterval: idleInterval,
		busyInterval: busyInterval,
		metrics:      metrics,
	}
}

// Run relays outbox rows until ctx is cancelled.
func (r *OutboxRelay) Run(ctx context.Context) {
	for {
		claimed := r.tick(ctx)
		delay := r.idleInterval
		if claimed >= outboxBatchSize {
			delay = r.busyInterval
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *OutboxRelay) tick(ctx context.Context) int {
	messages, err := r.store.ClaimOutbox(ctx, r.lease, outboxBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "claim outbox failed", "error", err)
		}
		return 0
	}

	published := make([]int64, 0, len(messages))
	for _, message := range messages {
		if ctx.Err() != nil {
			return len(messages)
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
		return len(messages)
	}
	if r.metrics != nil {
		r.metrics.OutboxLag(lag)
	}
	return len(messages)
}

// NoopHandler acknowledges messages without side effects.
type NoopHandler struct{}

// Handle does nothing.
func (NoopHandler) Handle(context.Context, app.OutboxMessage) error {
	return nil
}
