package app

import (
	"context"
	"fmt"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

// HoldIDGenerator produces identifiers for new holds.
type HoldIDGenerator func() (string, error)

// Reservation implements the hold lifecycle use cases.
type Reservation struct {
	holds   HoldStore
	newID   HoldIDGenerator
	holdTTL time.Duration
}

// NewReservation builds the reservation use cases.
func NewReservation(holds HoldStore, newID HoldIDGenerator, holdTTL time.Duration) *Reservation {
	return &Reservation{holds: holds, newID: newID, holdTTL: holdTTL}
}

// Acquire takes a hold on a seat or returns the caller's existing live hold.
func (r *Reservation) Acquire(ctx context.Context, req AcquireRequest) (AcquireResult, error) {
	holdID, err := r.newID()
	if err != nil {
		return AcquireResult{}, fmt.Errorf("generate hold id: %w", err)
	}

	return r.holds.Acquire(ctx, AcquireParams{
		HoldID:  holdID,
		EventID: req.EventID,
		SeatID:  req.SeatID,
		UserID:  req.UserID,
		TTL:     r.holdTTL,
	})
}

// Release gives an active hold back.
func (r *Reservation) Release(ctx context.Context, holdID, userID string) error {
	return r.holds.Release(ctx, holdID, userID)
}

// Get returns one of the caller's holds.
func (r *Reservation) Get(ctx context.Context, holdID, userID string) (reservation.Hold, error) {
	return r.holds.Get(ctx, holdID, userID)
}
