package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/config"
	"github.com/zezao/ticket-checkout-system/internal/worker"
)

const invariantCheckInterval = time.Minute

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, cfg.PGMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	holds := postgres.NewHoldStore(pool)
	sweeper := worker.NewSweeper(holds, logger, cfg.SweepInterval)
	checker := worker.NewChecker(postgres.NewInvariantChecker(pool), logger, invariantCheckInterval)

	logger.Info("worker started",
		"sweep_interval", cfg.SweepInterval.String(),
		"invariant_check_interval", invariantCheckInterval.String(),
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sweeper.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		checker.Run(ctx)
	}()
	wg.Wait()

	logger.Info("worker stopped")
	return nil
}
