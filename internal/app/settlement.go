package app

import (
	"context"
	"math/rand"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

const (
	baseBackoff    = 2 * time.Second
	maxBackoff     = time.Minute
	overdueBackoff = 10 * time.Minute
)

// Settlement turns provider answers into guarded attempt updates.
type Settlement struct {
	store           SettlementStore
	provider        Provider
	providerTimeout time.Duration
	jitter          func(time.Duration) time.Duration
	now             func() time.Time
}

// NewSettlement builds the settlement use case.
func NewSettlement(store SettlementStore, provider Provider, providerTimeout time.Duration) *Settlement {
	return &Settlement{
		store:           store,
		provider:        provider,
		providerTimeout: providerTimeout,
		jitter:          jittered,
		now:             time.Now,
	}
}

// Settle calls the provider outside any transaction, then applies the result.
func (s *Settlement) Settle(ctx context.Context, attempt payment.Attempt) error {
	if !attempt.Open() {
		return nil
	}

	outcome, lastError := s.callProvider(ctx, attempt)
	params := SettleParams{
		AttemptID: attempt.ID,
		Outcome:   outcome,
		LastError: lastError,
	}
	if outcome == SettlementPending {
		afterDeadline := !s.now().Before(attempt.DeadlineAt)
		backoff := Backoff(attempt.Attempts, afterDeadline)
		if !afterDeadline {
			backoff = s.jitter(backoff)
		}
		params.Backoff = backoff
	}

	_, err := s.store.ApplyResult(ctx, params)
	return err
}

func (s *Settlement) callProvider(ctx context.Context, attempt payment.Attempt) (SettlementOutcome, string) {
	callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	defer cancel()

	req := ChargeRequest{
		IdempotencyKey: attempt.ID,
		AmountCents:    attempt.AmountCents,
		Currency:       attempt.Currency,
	}

	var (
		result ChargeResult
		err    error
	)
	switch attempt.Status {
	case payment.StatusCreated:
		result, err = s.provider.CreateCharge(callCtx, req)
	case payment.StatusPending:
		result, err = s.provider.GetCharge(callCtx, req)
		if err == nil && result.Status == ChargeUnknown {
			result, err = s.provider.CreateCharge(callCtx, req)
		}
	default:
		return SettlementPending, "attempt is not open"
	}
	if err != nil {
		return SettlementPending, err.Error()
	}

	switch result.Status {
	case ChargeSucceeded:
		return SettlementSucceeded, ""
	case ChargeDeclined:
		return SettlementDeclined, ""
	case ChargeProcessing:
		return SettlementPending, ""
	case ChargeUnknown:
		return SettlementPending, "provider outcome unknown"
	default:
		return SettlementPending, "unexpected provider status"
	}
}

// Backoff returns the next re-check delay. After the deadline it is fixed.
func Backoff(attempts int, afterDeadline bool) time.Duration {
	if afterDeadline {
		return overdueBackoff
	}
	if attempts < 1 {
		attempts = 1
	}

	delay := baseBackoff
	for i := 1; i < attempts && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

func jittered(delay time.Duration) time.Duration {
	factor := 0.8 + rand.Float64()*0.4
	value := time.Duration(float64(delay) * factor)
	if value > maxBackoff {
		return maxBackoff
	}
	return value
}
