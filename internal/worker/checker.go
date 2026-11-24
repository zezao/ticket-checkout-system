package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// InvariantMetrics records checker observations.
type InvariantMetrics interface {
	InvariantViolation()
}

// Checker periodically runs the data-level invariant queries.
type Checker struct {
	checker  app.InvariantChecker
	logger   *slog.Logger
	interval time.Duration
	metrics  InvariantMetrics
}

// NewChecker builds an invariant checker loop.
func NewChecker(
	checker app.InvariantChecker,
	logger *slog.Logger,
	interval time.Duration,
	metrics InvariantMetrics,
) *Checker {
	return &Checker{checker: checker, logger: logger, interval: interval, metrics: metrics}
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
		if c.metrics != nil {
			c.metrics.InvariantViolation()
		}
		c.logger.ErrorContext(ctx, "invariant violation",
			"invariant", violation.Name,
			"ids", violation.IDs,
		)
	}
}
