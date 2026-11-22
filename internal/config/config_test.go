package config

import (
	"testing"
	"time"
)

func lookupFrom(vars map[string]string) func(string) string {
	return func(key string) string {
		return vars[key]
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := load(lookupFrom(map[string]string{
		"DATABASE_URL": "postgres://localhost/tickets",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.DatabaseURL != "postgres://localhost/tickets" {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, "postgres://localhost/tickets")
	}
	if cfg.RedisURL != "" {
		t.Errorf("RedisURL = %q, want empty", cfg.RedisURL)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.HoldTTL != 10*time.Minute {
		t.Errorf("HoldTTL = %v, want %v", cfg.HoldTTL, 10*time.Minute)
	}
	if cfg.PaymentDeadline != 15*time.Minute {
		t.Errorf("PaymentDeadline = %v, want %v", cfg.PaymentDeadline, 15*time.Minute)
	}
	if cfg.PaymentLease != 30*time.Second {
		t.Errorf("PaymentLease = %v, want %v", cfg.PaymentLease, 30*time.Second)
	}
	if cfg.ProviderTimeout != 10*time.Second {
		t.Errorf("ProviderTimeout = %v, want %v", cfg.ProviderTimeout, 10*time.Second)
	}
	if cfg.SweepInterval != 5*time.Second {
		t.Errorf("SweepInterval = %v, want %v", cfg.SweepInterval, 5*time.Second)
	}
	if cfg.PGMaxConns != 25 {
		t.Errorf("PGMaxConns = %d, want %d", cfg.PGMaxConns, 25)
	}
	if cfg.MockBehavior != "success" {
		t.Errorf("MockBehavior = %q, want %q", cfg.MockBehavior, "success")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Parallel()

	if _, err := load(lookupFrom(nil)); err == nil {
		t.Fatal("expected an error for a missing DATABASE_URL")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := load(lookupFrom(map[string]string{
		"DATABASE_URL":     "postgres://db/tickets",
		"REDIS_URL":        "redis://redis:6379",
		"HTTP_ADDR":        ":9090",
		"HOLD_TTL":         "90s",
		"PAYMENT_DEADLINE": "20m",
		"PAYMENT_LEASE":    "45s",
		"PROVIDER_TIMEOUT": "5s",
		"SWEEP_INTERVAL":   "2s",
		"PG_MAX_CONNS":     "50",
		"MOCK_BEHAVIOR":    "decline",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.DatabaseURL != "postgres://db/tickets" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.RedisURL != "redis://redis:6379" {
		t.Errorf("RedisURL = %q", cfg.RedisURL)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.HoldTTL != 90*time.Second {
		t.Errorf("HoldTTL = %v", cfg.HoldTTL)
	}
	if cfg.PaymentDeadline != 20*time.Minute {
		t.Errorf("PaymentDeadline = %v", cfg.PaymentDeadline)
	}
	if cfg.PaymentLease != 45*time.Second {
		t.Errorf("PaymentLease = %v", cfg.PaymentLease)
	}
	if cfg.ProviderTimeout != 5*time.Second {
		t.Errorf("ProviderTimeout = %v", cfg.ProviderTimeout)
	}
	if cfg.SweepInterval != 2*time.Second {
		t.Errorf("SweepInterval = %v", cfg.SweepInterval)
	}
	if cfg.PGMaxConns != 50 {
		t.Errorf("PGMaxConns = %d", cfg.PGMaxConns)
	}
	if cfg.MockBehavior != "decline" {
		t.Errorf("MockBehavior = %q", cfg.MockBehavior)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vars map[string]string
	}{
		{name: "hold ttl not a duration", vars: map[string]string{"HOLD_TTL": "banana"}},
		{name: "hold ttl zero", vars: map[string]string{"HOLD_TTL": "0s"}},
		{name: "hold ttl negative", vars: map[string]string{"HOLD_TTL": "-1m"}},
		{name: "payment deadline invalid", vars: map[string]string{"PAYMENT_DEADLINE": "soon"}},
		{name: "payment lease zero", vars: map[string]string{"PAYMENT_LEASE": "0s"}},
		{name: "provider timeout invalid", vars: map[string]string{"PROVIDER_TIMEOUT": "10"}},
		{name: "sweep interval negative", vars: map[string]string{"SWEEP_INTERVAL": "-5s"}},
		{name: "pg max conns not a number", vars: map[string]string{"PG_MAX_CONNS": "many"}},
		{name: "pg max conns zero", vars: map[string]string{"PG_MAX_CONNS": "0"}},
		{name: "pg max conns negative", vars: map[string]string{"PG_MAX_CONNS": "-1"}},
		{name: "unknown mock behavior", vars: map[string]string{"MOCK_BEHAVIOR": "sometimes"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vars := map[string]string{"DATABASE_URL": "postgres://db/tickets"}
			for key, value := range tt.vars {
				vars[key] = value
			}

			if _, err := load(lookupFrom(vars)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
