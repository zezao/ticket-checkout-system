package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/adapters/mockpay"
	"github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/config"
	"github.com/zezao/ticket-checkout-system/internal/worker"
)

const (
	invariantCheckInterval = time.Minute
	settlementInterval     = time.Second
)

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

	providerPool, err := postgres.NewPool(ctx, cfg.DatabaseURL, 10)
	if err != nil {
		return err
	}
	defer providerPool.Close()

	holds := postgres.NewHoldStore(pool)
	payments := postgres.NewPaymentStore(pool)
	provider, err := mockpay.New(providerPool, cfg.MockBehavior)
	if err != nil {
		return err
	}
	settlement := app.NewSettlement(payments, provider, cfg.ProviderTimeout)

	sweeper := worker.NewSweeper(holds, logger, cfg.SweepInterval)
	checker := worker.NewChecker(postgres.NewInvariantChecker(pool), logger, invariantCheckInterval)
	settler := worker.NewSettler(payments, settlement, logger, cfg.PaymentLease, settlementInterval)

	logger.Info("worker started",
		"sweep_interval", cfg.SweepInterval.String(),
		"settlement_interval", settlementInterval.String(),
		"payment_lease", cfg.PaymentLease.String(),
		"invariant_check_interval", invariantCheckInterval.String(),
		"mock_behavior", cfg.MockBehavior,
	)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		sweeper.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		checker.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		settler.Run(ctx)
	}()
	wg.Wait()

	logger.Info("worker stopped")
	return nil
}
