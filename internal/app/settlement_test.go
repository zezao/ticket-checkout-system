package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

type providerCall struct {
	result ChargeResult
	err    error
}

type stubProvider struct {
	createCalls []providerCall
	getCalls    []providerCall
	createSeen  []ChargeRequest
	getSeen     []ChargeRequest
}

func (p *stubProvider) CreateCharge(_ context.Context, req ChargeRequest) (ChargeResult, error) {
	p.createSeen = append(p.createSeen, req)
	return p.call(p.createCalls, len(p.createSeen)-1)
}

func (p *stubProvider) GetCharge(_ context.Context, req ChargeRequest) (ChargeResult, error) {
	p.getSeen = append(p.getSeen, req)
	return p.call(p.getCalls, len(p.getSeen)-1)
}

func (p *stubProvider) call(calls []providerCall, index int) (ChargeResult, error) {
	if index < len(calls) {
		return calls[index].result, calls[index].err
	}
	return ChargeResult{Status: ChargeUnknown}, nil
}

type stubSettlementStore struct {
	params []SettleParams
	result SettleResult
	err    error
}

func (s *stubSettlementStore) ClaimDue(context.Context, time.Duration, int) ([]payment.Attempt, error) {
	return nil, nil
}

func (s *stubSettlementStore) ApplyResult(_ context.Context, params SettleParams) (SettleResult, error) {
	s.params = append(s.params, params)
	return s.result, s.err
}

func (s *stubSettlementStore) CountOverdue(context.Context) (int, error) {
	return 0, nil
}

func newSettlementForTest(store SettlementStore, provider Provider) *Settlement {
	settlement := NewSettlement(store, provider, time.Second)
	settlement.jitter = func(delay time.Duration) time.Duration { return delay }
	return settlement
}

func openAttempt(status payment.Status) payment.Attempt {
	return payment.Attempt{
		ID:          "attempt-1",
		Status:      status,
		AmountCents: 8500,
		Currency:    "USD",
		Attempts:    1,
		DeadlineAt:  time.Now().Add(15 * time.Minute),
	}
}

func TestSettlementCreatedSucceeded(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{createCalls: []providerCall{
		{result: ChargeResult{Status: ChargeSucceeded}},
	}}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusCreated)); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if len(store.params) != 1 {
		t.Fatalf("applied %d results, want 1", len(store.params))
	}
	if got := store.params[0].Outcome; got != SettlementSucceeded {
		t.Errorf("outcome = %q, want %q", got, SettlementSucceeded)
	}
	if len(provider.createSeen) != 1 {
		t.Fatalf("CreateCharge calls = %d, want 1", len(provider.createSeen))
	}
	if provider.createSeen[0].IdempotencyKey != "attempt-1" {
		t.Errorf("idempotency key = %q", provider.createSeen[0].IdempotencyKey)
	}
}

func TestSettlementCreatedDeclined(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{createCalls: []providerCall{
		{result: ChargeResult{Status: ChargeDeclined}},
	}}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusCreated)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := store.params[0].Outcome; got != SettlementDeclined {
		t.Errorf("outcome = %q, want %q", got, SettlementDeclined)
	}
}

func TestSettlementProviderErrorBecomesPending(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{createCalls: []providerCall{
		{err: errors.New("provider timeout")},
	}}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusCreated)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	params := store.params[0]
	if params.Outcome != SettlementPending {
		t.Errorf("outcome = %q, want pending", params.Outcome)
	}
	if params.LastError != "provider timeout" {
		t.Errorf("last error = %q", params.LastError)
	}
	if params.Backoff != 2*time.Second {
		t.Errorf("backoff = %v, want 2s", params.Backoff)
	}
}

func TestSettlementProcessingBecomesPending(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{createCalls: []providerCall{
		{result: ChargeResult{Status: ChargeProcessing}},
	}}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusCreated)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := store.params[0].Outcome; got != SettlementPending {
		t.Errorf("outcome = %q, want pending", got)
	}
}

func TestSettlementPendingUsesGetCharge(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{getCalls: []providerCall{
		{result: ChargeResult{Status: ChargeSucceeded}},
	}}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusPending)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if len(provider.getSeen) != 1 {
		t.Fatalf("GetCharge calls = %d, want 1", len(provider.getSeen))
	}
	if len(provider.createSeen) != 0 {
		t.Errorf("CreateCharge calls = %d, want 0", len(provider.createSeen))
	}
	if got := store.params[0].Outcome; got != SettlementSucceeded {
		t.Errorf("outcome = %q, want succeeded", got)
	}
}

func TestSettlementPendingReissuesCreateWhenUnknown(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{
		getCalls: []providerCall{
			{result: ChargeResult{Status: ChargeUnknown}},
		},
		createCalls: []providerCall{
			{result: ChargeResult{Status: ChargeSucceeded}},
		},
	}
	settlement := newSettlementForTest(store, provider)

	if err := settlement.Settle(context.Background(), openAttempt(payment.StatusPending)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if len(provider.createSeen) != 1 {
		t.Errorf("CreateCharge calls = %d, want 1", len(provider.createSeen))
	}
	if got := store.params[0].Outcome; got != SettlementSucceeded {
		t.Errorf("outcome = %q, want succeeded", got)
	}
}

func TestSettlementAfterDeadlineUsesFixedBackoff(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{createCalls: []providerCall{
		{result: ChargeResult{Status: ChargeProcessing}},
	}}
	settlement := newSettlementForTest(store, provider)

	attempt := openAttempt(payment.StatusCreated)
	attempt.DeadlineAt = time.Now().Add(-time.Minute)

	if err := settlement.Settle(context.Background(), attempt); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := store.params[0].Backoff; got != 10*time.Minute {
		t.Errorf("backoff = %v, want 10m", got)
	}
}

func TestSettlementSkipsFinalAttempt(t *testing.T) {
	t.Parallel()

	store := &stubSettlementStore{}
	provider := &stubProvider{}
	settlement := newSettlementForTest(store, provider)

	for _, status := range []payment.Status{payment.StatusSucceeded, payment.StatusFailed} {
		attempt := openAttempt(status)
		if err := settlement.Settle(context.Background(), attempt); err != nil {
			t.Fatalf("Settle(%s): %v", status, err)
		}
	}
	if len(provider.createSeen)+len(provider.getSeen) != 0 {
		t.Error("provider was called for a final attempt")
	}
	if len(store.params) != 0 {
		t.Error("store was called for a final attempt")
	}
}

func TestBackoffSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 2 * time.Second},
		{attempts: 1, want: 2 * time.Second},
		{attempts: 2, want: 4 * time.Second},
		{attempts: 3, want: 8 * time.Second},
		{attempts: 4, want: 16 * time.Second},
		{attempts: 5, want: 32 * time.Second},
		{attempts: 6, want: time.Minute},
		{attempts: 100, want: time.Minute},
	}
	for _, tt := range tests {
		if got := Backoff(tt.attempts, false); got != tt.want {
			t.Errorf("Backoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
	if got := Backoff(1, true); got != 10*time.Minute {
		t.Errorf("Backoff after deadline = %v, want 10m", got)
	}
}

func TestJitteredStaysInRange(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		got := jittered(10 * time.Second)
		if got < 8*time.Second || got > 12*time.Second {
			t.Fatalf("jittered(10s) = %v, want [8s, 12s]", got)
		}
		if got := jittered(time.Minute); got > time.Minute {
			t.Fatalf("jittered(1m) = %v, want at most 1m", got)
		}
	}
}
