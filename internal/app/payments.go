package app

import (
	"context"
	"fmt"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
)

// PaymentIDGenerator produces identifiers for new payment attempts.
type PaymentIDGenerator func() (string, error)

// Payments implements the start and read payment use cases.
type Payments struct {
	store    PaymentStore
	newID    PaymentIDGenerator
	deadline time.Duration
}

// NewPayments builds the payment use cases.
func NewPayments(store PaymentStore, newID PaymentIDGenerator, deadline time.Duration) *Payments {
	return &Payments{store: store, newID: newID, deadline: deadline}
}

// Start creates or returns the attempt for one hold and idempotency key.
func (p *Payments) Start(ctx context.Context, req StartPaymentRequest) (payment.Attempt, error) {
	attemptID, err := p.newID()
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("generate payment id: %w", err)
	}

	return p.store.Start(ctx, StartPaymentParams{
		AttemptID:      attemptID,
		HoldID:         req.HoldID,
		UserID:         req.UserID,
		IdempotencyKey: req.IdempotencyKey,
		Traceparent:    req.Traceparent,
		Deadline:       p.deadline,
	})
}

// GetForHold returns the caller's attempt for a hold, if one exists.
func (p *Payments) GetForHold(ctx context.Context, holdID, userID string) (payment.Attempt, bool, error) {
	return p.store.GetForHold(ctx, holdID, userID)
}
