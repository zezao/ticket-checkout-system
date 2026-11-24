package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

const sweepBatchSize = 100

// SweepMetrics records sweeper observations.
type SweepMetrics interface {
	ConsistencyError()
}

// Sweeper periodically retires expired active holds.
type Sweeper struct {
	store    app.ExpiryStore
	logger   *slog.Logger
	interval time.Duration
	metrics  SweepMetrics
}

// NewSweeper builds an expiry sweeper.
func NewSweeper(
	store app.ExpiryStore,
	logger *slog.Logger,
	interval time.Duration,
	metrics SweepMetrics,
) *Sweeper {
	return &Sweeper{store: store, logger: logger, interval: interval, metrics: metrics}
}

// Run sweeps until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	s.sweep(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	expired, err := s.store.ExpireBatch(ctx, sweepBatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if s.metrics != nil && errors.Is(err, app.ErrConsistency) {
			s.metrics.ConsistencyError()
		}
		s.logger.ErrorContext(ctx, "expiry sweep failed", "error", err)
		return
	}
	if expired > 0 {
		s.logger.InfoContext(ctx, "expired holds", "count", expired)
	}
}
