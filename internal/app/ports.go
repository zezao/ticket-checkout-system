package app

import (
	"context"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

// Event is the read model for an event.
type Event struct {
	ID       string
	Name     string
	Venue    string
	StartsAt time.Time
	Currency string
}

// EventStore reads events.
type EventStore interface {
	ListEvents(ctx context.Context) ([]Event, error)
	GetEvent(ctx context.Context, id string) (Event, error)
}

// SeatStore reads seats.
type SeatStore interface {
	ListByEvent(ctx context.Context, eventID string) ([]reservation.Seat, error)
}

// SeatMapCache caches the seats of one event. Cache errors are never fatal.
type SeatMapCache interface {
	Get(ctx context.Context, eventID string) ([]reservation.Seat, bool, error)
	Set(ctx context.Context, eventID string, seats []reservation.Seat) error
	Delete(ctx context.Context, eventID string) error
}

// AcquireRequest is the input to the acquire use case.
type AcquireRequest struct {
	EventID string
	SeatID  string
	UserID  string
}

// AcquireParams is the atomic operation the hold store performs.
type AcquireParams struct {
	HoldID  string
	EventID string
	SeatID  string
	UserID  string
	TTL     time.Duration
}

// AcquireResult reports the hold and whether this call created it.
type AcquireResult struct {
	Hold    reservation.Hold
	Created bool
}

// HoldStore owns hold persistence. Acquire and Release are atomic.
type HoldStore interface {
	Acquire(ctx context.Context, params AcquireParams) (AcquireResult, error)
	Release(ctx context.Context, holdID, userID string) error
	Get(ctx context.Context, holdID, userID string) (reservation.Hold, error)
}

// ExpiryStore retires expired holds in batches.
type ExpiryStore interface {
	ExpireBatch(ctx context.Context, limit int) (int, error)
}

// ChargeRequest is one provider call for one attempt.
type ChargeRequest struct {
	IdempotencyKey string
	AmountCents    int64
	Currency       string
}

// ChargeStatus is the provider's answer for one call.
type ChargeStatus string

const (
	ChargeSucceeded  ChargeStatus = "succeeded"
	ChargeDeclined   ChargeStatus = "declined"
	ChargeProcessing ChargeStatus = "processing"
	ChargeUnknown    ChargeStatus = "unknown"
)

// ChargeResult is the provider's answer without transport details.
type ChargeResult struct {
	Status ChargeStatus
}

// Provider is the payment provider port. Both calls are idempotent on the key.
type Provider interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
	GetCharge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

// StartPaymentRequest is the input to the start payment use case.
type StartPaymentRequest struct {
	HoldID         string
	UserID         string
	IdempotencyKey string
	Traceparent    string
}

// StartPaymentParams is the atomic operation the payment store performs.
type StartPaymentParams struct {
	AttemptID      string
	HoldID         string
	UserID         string
	IdempotencyKey string
	Traceparent    string
	Deadline       time.Duration
}

// PaymentStore owns the payment attempt reads and the start transition.
type PaymentStore interface {
	Start(ctx context.Context, params StartPaymentParams) (payment.Attempt, error)
	GetForHold(ctx context.Context, holdID, userID string) (payment.Attempt, bool, error)
}

// SettlementOutcome is the outcome settlement applies to an attempt.
type SettlementOutcome string

const (
	SettlementSucceeded SettlementOutcome = "succeeded"
	SettlementDeclined  SettlementOutcome = "declined"
	SettlementPending   SettlementOutcome = "pending"
)

// SettleParams is one guarded settlement update.
type SettleParams struct {
	AttemptID string
	Outcome   SettlementOutcome
	LastError string
	Backoff   time.Duration
}

// SettleResult reports whether the attempt was still open.
type SettleResult struct {
	Applied bool
	Attempt payment.Attempt
}

// SettlementStore claims due attempts and applies provider results atomically.
type SettlementStore interface {
	ClaimDue(ctx context.Context, lease time.Duration, limit int) ([]payment.Attempt, error)
	ApplyResult(ctx context.Context, params SettleParams) (SettleResult, error)
	CountOverdue(ctx context.Context) (int, error)
}

// InvariantViolation describes one failed data invariant.
type InvariantViolation struct {
	Name string
	IDs  []string
}

// InvariantChecker reports data-level invariant violations without repairing them.
type InvariantChecker interface {
	Check(ctx context.Context) ([]InvariantViolation, error)
}

// OutboxMessage is one unpublished event claimed by the relay.
type OutboxMessage struct {
	ID          int64
	Topic       string
	Payload     []byte
	Traceparent string
}

// OutboxStore claims and acknowledges outbox rows.
type OutboxStore interface {
	ClaimOutbox(ctx context.Context, lease time.Duration, limit int) ([]OutboxMessage, error)
	MarkPublished(ctx context.Context, ids []int64) error
	OutboxLag(ctx context.Context) (time.Duration, error)
}
