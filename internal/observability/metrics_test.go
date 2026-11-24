package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposeExpectedSeries(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics()
	metrics.HoldAttempt("acquired", 10*time.Millisecond)
	metrics.HoldAttempt("conflict", 5*time.Millisecond)
	metrics.Payment("succeeded")
	metrics.Overdue(2)
	metrics.OutboxLag(3 * time.Second)
	metrics.InvariantViolation()
	metrics.ConsistencyError()
	metrics.SeatMapCache("hit")
	metrics.RedisError()

	if err := metrics.RegisterPool(
		"app",
		func() int { return 3 },
		func() float64 { return 1.5 },
	); err != nil {
		t.Fatalf("RegisterPool: %v", err)
	}

	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	wants := []string{
		`hold_attempts_total{result="acquired"} 1`,
		`hold_attempts_total{result="conflict"} 1`,
		`hold_acquire_duration_seconds_count 2`,
		`payments_total{result="succeeded"} 1`,
		`payments_overdue 2`,
		`outbox_lag_seconds 3`,
		`invariant_violations_total 1`,
		`consistency_errors_total 1`,
		`seatmap_cache_total{result="hit"} 1`,
		`redis_errors_total 1`,
		`pgxpool_acquired_conns{pool="app"} 3`,
		`pgxpool_acquire_wait_seconds_total{pool="app"} 1.5`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output does not contain %q", want)
		}
	}
}

func TestRegisterPoolRejectsDuplicateName(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics()
	if err := metrics.RegisterPool("app", func() int { return 0 }, func() float64 { return 0 }); err != nil {
		t.Fatalf("first RegisterPool: %v", err)
	}
	if err := metrics.RegisterPool("app", func() int { return 0 }, func() float64 { return 0 }); err == nil {
		t.Fatal("expected a duplicate registration error")
	}
}
