package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/zezao/ticket-checkout-system/internal/adapters/httpapi"
	"github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	redisadapter "github.com/zezao/ticket-checkout-system/internal/adapters/redis"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/config"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api exited with error", "error", err)
		os.Exit(1)
	}
}

func newUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := observability.SetupTracing(ctx, "ticket-checkout-api")
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

	pgCatalog := postgres.NewCatalog(pool)

	metrics := observability.NewMetrics()
	if err := metrics.RegisterPool(
		"app",
		func() int { return int(pool.Stat().AcquiredConns()) },
		func() float64 { return pool.Stat().EmptyAcquireWaitTime().Seconds() },
	); err != nil {
		return err
	}

	var seatStore app.SeatStore = pgCatalog
	var (
		rateLimiter httpapi.RateLimiter
		redisPinger httpapi.Pinger
	)
	if cfg.RedisURL != "" {
		redisClient, err := redisadapter.NewClient(cfg.RedisURL)
		if err != nil {
			return err
		}
		defer func() { _ = redisClient.Close() }()

		seatStore = redisadapter.NewCachedSeats(
			pgCatalog,
			redisadapter.NewSeatMapCache(redisClient),
			metrics,
		)
		rateLimiter = redisadapter.NewLimiter(redisClient, cfg.HoldRateLimit, metrics)
		redisPinger = redisadapter.NewPinger(redisClient)
	}

	catalog := app.NewCatalog(pgCatalog, seatStore)
	holds := postgres.NewHoldStore(pool)
	reservations := app.NewReservation(holds, newUUIDv7, cfg.HoldTTL)
	payments := app.NewPayments(postgres.NewPaymentStore(pool), newUUIDv7, cfg.PaymentDeadline)

	handler := httpapi.NewHandler(httpapi.Deps{
		Postgres:       pool,
		Redis:          redisPinger,
		Catalog:        catalog,
		Reservations:   reservations,
		Payments:       payments,
		RateLimiter:    rateLimiter,
		Metrics:        metrics,
		MetricsHandler: metrics.Handler(),
		Logger:         logger,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           observability.HTTPHandler(handler, "api"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger.Info("api shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("api stopped")
	return nil
}
