package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

type stubInvariantChecker struct {
	violations []app.InvariantViolation
	err        error
	calls      int
}

func (s *stubInvariantChecker) Check(context.Context) ([]app.InvariantViolation, error) {
	s.calls++
	return s.violations, s.err
}

func TestCheckerChecks(t *testing.T) {
	t.Parallel()

	checker := &stubInvariantChecker{violations: []app.InvariantViolation{
		{Name: "seat_missing_hold", IDs: []string{"seat-1"}},
	}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	loop := NewChecker(checker, logger, time.Minute, nil)
	loop.check(context.Background())

	if checker.calls != 1 {
		t.Errorf("calls = %d, want 1", checker.calls)
	}
	logged := buf.String()
	if !strings.Contains(logged, "seat_missing_hold") {
		t.Errorf("log line %q does not name the invariant", logged)
	}
	if !strings.Contains(logged, "seat-1") {
		t.Errorf("log line %q does not include the offending id", logged)
	}
}

func TestCheckerHandlesError(t *testing.T) {
	t.Parallel()

	checker := &stubInvariantChecker{err: errors.New("database unavailable")}
	loop := NewChecker(checker, workerTestLogger(), time.Minute, nil)

	loop.check(context.Background())

	if checker.calls != 1 {
		t.Errorf("calls = %d, want 1", checker.calls)
	}
}

type stubInvariantMetrics struct {
	checks int
}

func (m *stubInvariantMetrics) InvariantViolation() {
	m.checks++
}

func TestCheckerRecordsViolationMetrics(t *testing.T) {
	t.Parallel()

	checker := &stubInvariantChecker{violations: []app.InvariantViolation{
		{Name: "seat_missing_hold"},
		{Name: "live_hold_missing_seat"},
	}}
	metrics := &stubInvariantMetrics{}
	loop := NewChecker(checker, workerTestLogger(), time.Minute, metrics)

	loop.check(context.Background())

	if metrics.checks != 2 {
		t.Errorf("invariant violation metrics = %d, want 2", metrics.checks)
	}
}
