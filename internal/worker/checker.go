package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// Checker periodically runs the data-level invariant queries.
type Checker struct {
	checker  app.InvariantChecker
	logger   *slog.Logger
	interval time.Duration
}

// NewChecker builds an invariant checker loop.
func NewChecker(checker app.InvariantChecker, logger *slog.Logger, interval time.Duration) *Checker {
	return &Checker{checker: checker, logger: logger, interval: interval}
}

// Run checks invariants until ctx is cancelled.
func (c *Checker) Run(ctx context.Context) {
	c.check(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check(ctx)
		}
	}
}

func (c *Checker) check(ctx context.Context) {
	violations, err := c.checker.Check(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.logger.ErrorContext(ctx, "invariant check failed", "error", err)
		return
	}

	for _, violation := range violations {
		c.logger.ErrorContext(ctx, "invariant violation",
			"invariant", violation.Name,
			"ids", violation.IDs,
		)
	}
}
