package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

const settlementBatchSize = 100

// Settlement applies one provider result to one claimed attempt.
type Settlement interface {
	Settle(ctx context.Context, attempt payment.Attempt) (app.SettlementOutcome, error)
}

// SettlementMetrics records payment settlement observations.
type SettlementMetrics interface {
	Payment(result string)
	Overdue(count int)
	ConsistencyError()
}

// Settler claims due attempts and runs settlement against the provider.
type Settler struct {
	store      app.SettlementStore
	settlement Settlement
	logger     *slog.Logger
	lease      time.Duration
	interval   time.Duration
	metrics    SettlementMetrics
}

// NewSettler builds the payment settlement loop.
func NewSettler(
	store app.SettlementStore,
	settlement Settlement,
	logger *slog.Logger,
	lease, interval time.Duration,
	metrics SettlementMetrics,
) *Settler {
	return &Settler{
		store:      store,
		settlement: settlement,
		logger:     logger,
		lease:      lease,
		interval:   interval,
		metrics:    metrics,
	}
}

// Run settles due attempts until ctx is cancelled.
func (s *Settler) Run(ctx context.Context) {
	s.tick(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Settler) tick(ctx context.Context) {
	attempts, err := s.store.ClaimDue(ctx, s.lease, settlementBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "claim due payments failed", "error", err)
		}
		return
	}

	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return
		}

		settleCtx, span := observability.ContinueTrace(ctx, attempt.Traceparent, "settle_payment")
		outcome, err := s.settlement.Settle(settleCtx, attempt)
		span.End()
		if err != nil {
			if s.metrics != nil {
				s.metrics.Payment("error")
				if errors.Is(err, app.ErrConsistency) {
					s.metrics.ConsistencyError()
				}
			}
			s.logger.ErrorContext(ctx, "settle payment failed",
				"attempt_id", attempt.ID,
				"hold_id", attempt.HoldID,
				"error", err,
			)
			continue
		}
		if outcome != "" && s.metrics != nil {
			s.metrics.Payment(string(outcome))
		}
	}

	overdue, err := s.store.CountOverdue(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "count overdue payments failed", "error", err)
		}
		return
	}
	if s.metrics != nil {
		s.metrics.Overdue(overdue)
	}
	if overdue > 0 {
		s.logger.WarnContext(ctx, "payments overdue", "count", overdue)
	}
}
