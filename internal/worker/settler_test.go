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
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

type stubSettlerStore struct {
	attempts     []payment.Attempt
	claimErr     error
	claimCalls   int
	overdue      int
	overdueErr   error
	overdueCalls int
}

func (s *stubSettlerStore) ClaimDue(context.Context, time.Duration, int) ([]payment.Attempt, error) {
	s.claimCalls++
	return s.attempts, s.claimErr
}

func (s *stubSettlerStore) ApplyResult(context.Context, app.SettleParams) (app.SettleResult, error) {
	return app.SettleResult{}, nil
}

func (s *stubSettlerStore) CountOverdue(context.Context) (int, error) {
	s.overdueCalls++
	return s.overdue, s.overdueErr
}

type stubSettlement struct {
	seen []payment.Attempt
	err  error
}

func (s *stubSettlement) Settle(_ context.Context, attempt payment.Attempt) error {
	s.seen = append(s.seen, attempt)
	return s.err
}

func TestSettlerTick(t *testing.T) {
	t.Parallel()

	attempts := []payment.Attempt{{ID: "attempt-1"}, {ID: "attempt-2"}}
	store := &stubSettlerStore{attempts: attempts, overdue: 2}
	settlement := &stubSettlement{}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	settler := NewSettler(store, settlement, logger, time.Second, time.Minute)
	settler.tick(context.Background())

	if store.claimCalls != 1 {
		t.Errorf("claim calls = %d, want 1", store.claimCalls)
	}
	if len(settlement.seen) != 2 {
		t.Fatalf("settled %d attempts, want 2", len(settlement.seen))
	}
	if store.overdueCalls != 1 {
		t.Errorf("overdue calls = %d, want 1", store.overdueCalls)
	}
	if !strings.Contains(buf.String(), "payments overdue") {
		t.Errorf("log line %q does not warn about overdue payments", buf.String())
	}
}

func TestSettlerContinuesAfterSettlementError(t *testing.T) {
	t.Parallel()

	store := &stubSettlerStore{attempts: []payment.Attempt{{ID: "attempt-1"}, {ID: "attempt-2"}}}
	settlement := &stubSettlement{err: errors.New("database unavailable")}

	settler := NewSettler(store, settlement, workerTestLogger(), time.Second, time.Minute)
	settler.tick(context.Background())

	if len(settlement.seen) != 2 {
		t.Errorf("settled %d attempts, want 2", len(settlement.seen))
	}
}

func TestSettlerClaimErrorStopsTick(t *testing.T) {
	t.Parallel()

	store := &stubSettlerStore{claimErr: errors.New("database unavailable")}
	settlement := &stubSettlement{}

	settler := NewSettler(store, settlement, workerTestLogger(), time.Second, time.Minute)
	settler.tick(context.Background())

	if len(settlement.seen) != 0 {
		t.Error("settlement ran after a claim error")
	}
	if store.overdueCalls != 0 {
		t.Errorf("overdue calls = %d, want 0", store.overdueCalls)
	}
}
