package app

import (
	"context"
	"time"

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

// InvariantViolation describes one failed data invariant.
type InvariantViolation struct {
	Name string
	IDs  []string
}

// InvariantChecker reports data-level invariant violations without repairing them.
type InvariantChecker interface {
	Check(ctx context.Context) ([]InvariantViolation, error)
}
