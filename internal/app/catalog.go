package app

import (
	"context"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

// SeatMap is an event together with its seats.
type SeatMap struct {
	Event Event
	Seats []reservation.Seat
}

// Catalog serves the read-only event and seat queries.
type Catalog struct {
	events EventStore
	seats  SeatStore
}

// NewCatalog builds a Catalog from its stores.
func NewCatalog(events EventStore, seats SeatStore) *Catalog {
	return &Catalog{events: events, seats: seats}
}

// ListEvents returns every event ordered by start time.
func (c *Catalog) ListEvents(ctx context.Context) ([]Event, error) {
	return c.events.ListEvents(ctx)
}

// GetEvent returns one event.
func (c *Catalog) GetEvent(ctx context.Context, id string) (Event, error) {
	return c.events.GetEvent(ctx, id)
}

// SeatMap returns an event and the seats that belong to it.
func (c *Catalog) SeatMap(ctx context.Context, eventID string) (SeatMap, error) {
	event, err := c.events.GetEvent(ctx, eventID)
	if err != nil {
		return SeatMap{}, err
	}

	seats, err := c.seats.ListByEvent(ctx, eventID)
	if err != nil {
		return SeatMap{}, err
	}

	return SeatMap{Event: event, Seats: seats}, nil
}
