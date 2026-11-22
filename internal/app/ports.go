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
