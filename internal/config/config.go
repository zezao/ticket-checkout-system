// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultHoldTTL         = 10 * time.Minute
	defaultPaymentDeadline = 15 * time.Minute
	defaultPaymentLease    = 30 * time.Second
	defaultProviderTimeout = 10 * time.Second
	defaultSweepInterval   = 5 * time.Second
	defaultPGMaxConns      = 25
	defaultHTTPAddr        = ":8080"
	defaultMockBehavior    = "success"
)

var mockBehaviors = map[string]struct{}{
	"success":              {},
	"decline":              {},
	"timeout_after_charge": {},
	"unresolved":           {},
}

// Config holds the runtime settings shared by the api and worker binaries.
type Config struct {
	DatabaseURL     string
	RedisURL        string
	HTTPAddr        string
	HoldTTL         time.Duration
	PaymentDeadline time.Duration
	PaymentLease    time.Duration
	ProviderTimeout time.Duration
	SweepInterval   time.Duration
	PGMaxConns      int32
	MockBehavior    string
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(lookup func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL:  lookup("DATABASE_URL"),
		RedisURL:     lookup("REDIS_URL"),
		HTTPAddr:     stringOr(lookup, "HTTP_ADDR", defaultHTTPAddr),
		MockBehavior: stringOr(lookup, "MOCK_BEHAVIOR", defaultMockBehavior),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL: required")
	}
	if _, ok := mockBehaviors[cfg.MockBehavior]; !ok {
		return Config{}, fmt.Errorf("MOCK_BEHAVIOR: unknown behavior %q", cfg.MockBehavior)
	}

	var err error
	if cfg.HoldTTL, err = duration(lookup, "HOLD_TTL", defaultHoldTTL); err != nil {
		return Config{}, err
	}
	if cfg.PaymentDeadline, err = duration(lookup, "PAYMENT_DEADLINE", defaultPaymentDeadline); err != nil {
		return Config{}, err
	}
	if cfg.PaymentLease, err = duration(lookup, "PAYMENT_LEASE", defaultPaymentLease); err != nil {
		return Config{}, err
	}
	if cfg.ProviderTimeout, err = duration(lookup, "PROVIDER_TIMEOUT", defaultProviderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SweepInterval, err = duration(lookup, "SWEEP_INTERVAL", defaultSweepInterval); err != nil {
		return Config{}, err
	}
	if cfg.PGMaxConns, err = int32Value(lookup, "PG_MAX_CONNS", defaultPGMaxConns); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func stringOr(lookup func(string) string, key, fallback string) string {
	if value := lookup(key); value != "" {
		return value
	}
	return fallback
}

func duration(lookup func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := lookup(key)
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s: must be positive", key)
	}
	return value, nil
}

func int32Value(lookup func(string) string, key string, fallback int) (int32, error) {
	raw := lookup(key)
	if raw == "" {
		return int32(fallback), nil
	}

	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s: must be positive", key)
	}
	return int32(value), nil
}
