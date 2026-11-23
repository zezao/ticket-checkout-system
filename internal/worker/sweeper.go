package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

const sweepBatchSize = 100

// Sweeper periodically retires expired active holds.
type Sweeper struct {
	store    app.ExpiryStore
	logger   *slog.Logger
	interval time.Duration
}

// NewSweeper builds an expiry sweeper.
func NewSweeper(store app.ExpiryStore, logger *slog.Logger, interval time.Duration) *Sweeper {
	return &Sweeper{store: store, logger: logger, interval: interval}
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
		s.logger.ErrorContext(ctx, "expiry sweep failed", "error", err)
		return
	}
	if expired > 0 {
		s.logger.InfoContext(ctx, "expired holds", "count", expired)
	}
}
