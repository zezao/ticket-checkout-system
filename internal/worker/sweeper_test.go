package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

type stubExpiryStore struct {
	expired int
	err     error
	calls   int
	limit   int
}

func (s *stubExpiryStore) ExpireBatch(_ context.Context, limit int) (int, error) {
	s.calls++
	s.limit = limit
	return s.expired, s.err
}

func TestSweeperSweeps(t *testing.T) {
	t.Parallel()

	store := &stubExpiryStore{expired: 3}
	sweeper := NewSweeper(store, workerTestLogger(), time.Minute, nil)

	sweeper.sweep(context.Background())

	if store.calls != 1 {
		t.Errorf("calls = %d, want 1", store.calls)
	}
	if store.limit != sweepBatchSize {
		t.Errorf("limit = %d, want %d", store.limit, sweepBatchSize)
	}
}

func TestSweeperHandlesError(t *testing.T) {
	t.Parallel()

	store := &stubExpiryStore{err: errors.New("database unavailable")}
	sweeper := NewSweeper(store, workerTestLogger(), time.Minute, nil)

	sweeper.sweep(context.Background())

	if store.calls != 1 {
		t.Errorf("calls = %d, want 1", store.calls)
	}
}

type stubSweepMetrics struct {
	consistencyErrors int
}

func (m *stubSweepMetrics) ConsistencyError() {
	m.consistencyErrors++
}

func TestSweeperRecordsConsistencyErrors(t *testing.T) {
	t.Parallel()

	store := &stubExpiryStore{err: app.ErrConsistency}
	metrics := &stubSweepMetrics{}
	sweeper := NewSweeper(store, workerTestLogger(), time.Minute, metrics)

	sweeper.sweep(context.Background())

	if metrics.consistencyErrors != 1 {
		t.Errorf("consistency errors = %d, want 1", metrics.consistencyErrors)
	}
}

func workerTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
