package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/adapters/mockpay"
	"github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	redisadapter "github.com/zezao/ticket-checkout-system/internal/adapters/redis"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/config"
	"github.com/zezao/ticket-checkout-system/internal/observability"
	"github.com/zezao/ticket-checkout-system/internal/worker"
)

const (
	invariantCheckInterval = time.Minute
	settlementInterval     = time.Second
	outboxInterval         = time.Second
	outboxLease            = 30 * time.Second
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

	shutdownTracing, err := observability.SetupTracing(ctx, "ticket-checkout-worker")
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

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

	metrics := observability.NewMetrics()
	if err := metrics.RegisterPool(
		"app",
		func() int { return int(pool.Stat().AcquiredConns()) },
		func() float64 { return pool.Stat().EmptyAcquireWaitTime().Seconds() },
	); err != nil {
		return err
	}
	if err := metrics.RegisterPool(
		"provider",
		func() int { return int(providerPool.Stat().AcquiredConns()) },
		func() float64 { return providerPool.Stat().EmptyAcquireWaitTime().Seconds() },
	); err != nil {
		return err
	}

	holds := postgres.NewHoldStore(pool)
	payments := postgres.NewPaymentStore(pool)
	provider, err := mockpay.New(providerPool, cfg.MockBehavior)
	if err != nil {
		return err
	}
	var providerPort app.Provider = provider
	providerPort = observability.NewTracedProvider(providerPort)
	settlement := app.NewSettlement(payments, providerPort, cfg.ProviderTimeout)

	outboxHandlers := map[string]worker.OutboxHandler{
		"seat.changed":    worker.NoopHandler{},
		"order.confirmed": worker.NoopHandler{},
	}
	if cfg.RedisURL != "" {
		redisClient, err := redisadapter.NewClient(cfg.RedisURL)
		if err != nil {
			return err
		}
		defer func() { _ = redisClient.Close() }()

		cache := redisadapter.NewSeatMapCache(redisClient)
		outboxHandlers["seat.changed"] = worker.NewSeatChangedHandler(cache, logger, metrics)
	}

	outboxStore := postgres.NewOutboxStore(pool)
	relay := worker.NewOutboxRelay(outboxStore, outboxHandlers, logger,
		outboxLease, outboxInterval, metrics)

	sweeper := worker.NewSweeper(holds, logger, cfg.SweepInterval, metrics)
	checker := worker.NewChecker(postgres.NewInvariantChecker(pool), logger,
		invariantCheckInterval, metrics)
	settler := worker.NewSettler(payments, settlement, logger,
		cfg.PaymentLease, settlementInterval, metrics)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metrics.Handler())
	metricsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("worker started",
		"sweep_interval", cfg.SweepInterval.String(),
		"settlement_interval", settlementInterval.String(),
		"outbox_interval", outboxInterval.String(),
		"payment_lease", cfg.PaymentLease.String(),
		"invariant_check_interval", invariantCheckInterval.String(),
		"metrics_addr", cfg.MetricsAddr,
		"mock_behavior", cfg.MockBehavior,
		"redis_enabled", cfg.RedisURL != "",
	)

	var wg sync.WaitGroup
	wg.Add(6)
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
	go func() {
		defer wg.Done()
		relay.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		logger.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics server shutdown failed", "error", err)
		}
	}()
	wg.Wait()

	logger.Info("worker stopped")
	return nil
}
