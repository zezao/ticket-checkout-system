package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

func TestInvariant01_ConcurrentHoldsOnOneSeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newPool(t)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)
	reservations := newReservation(pool)

	const attempts = 1000

	var (
		created     atomic.Int64
		unavailable atomic.Int64
		unexpected  atomic.Int64
		wg          sync.WaitGroup
	)
	errs := make(chan error, attempts)
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		userID := uuid.NewString()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			result, err := reservations.Acquire(ctx, app.AcquireRequest{
				EventID: eventID,
				SeatID:  seatID,
				UserID:  userID,
			})
			switch {
			case err == nil && result.Created:
				created.Add(1)
			case errors.Is(err, app.ErrSeatUnavailable):
				unavailable.Add(1)
			default:
				unexpected.Add(1)
				if err == nil {
					err = fmt.Errorf("acquire returned an existing hold")
				}
				errs <- err
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	if got := created.Load(); got != 1 {
		t.Errorf("created = %d, want 1", got)
	}
	if got := unavailable.Load(); got != attempts-1 {
		t.Errorf("seat_unavailable = %d, want %d", got, attempts-1)
	}
	if got := unexpected.Load(); got != 0 {
		logFirstErrors(t, errs)
		t.Fatalf("unexpected results = %d, want 0", got)
	}

	var liveHolds int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM holds WHERE seat_id = $1::uuid AND status = 'active'", seatID,
	).Scan(&liveHolds); err != nil {
		t.Fatalf("count live holds: %v", err)
	}
	if liveHolds != 1 {
		t.Errorf("live holds = %d, want 1", liveHolds)
	}

	checkInvariants(t, pool)
}

func TestInvariant01_ConcurrentHoldsAcrossSeats(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool := newPool(t)
	eventID := insertEvent(t, pool)
	reservations := newReservation(pool)

	const (
		seatCount       = 100
		attemptsPerSeat = 100
	)

	seatIDs := make([]string, seatCount)
	for i := range seatIDs {
		seatIDs[i] = insertSeat(t, pool, eventID, "GA", "A", i+1, 8500)
	}

	const attempts = seatCount * attemptsPerSeat

	var (
		created     atomic.Int64
		unavailable atomic.Int64
		unexpected  atomic.Int64
		wg          sync.WaitGroup
	)
	errs := make(chan error, attempts)
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		seatID := seatIDs[i%seatCount]
		userID := uuid.NewString()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			result, err := reservations.Acquire(ctx, app.AcquireRequest{
				EventID: eventID,
				SeatID:  seatID,
				UserID:  userID,
			})
			switch {
			case err == nil && result.Created:
				created.Add(1)
			case errors.Is(err, app.ErrSeatUnavailable):
				unavailable.Add(1)
			default:
				unexpected.Add(1)
				if err == nil {
					err = fmt.Errorf("acquire returned an existing hold")
				}
				errs <- err
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	if got := created.Load(); got != seatCount {
		t.Errorf("created = %d, want %d", got, seatCount)
	}
	if got := unavailable.Load(); got != attempts-seatCount {
		t.Errorf("seat_unavailable = %d, want %d", got, attempts-seatCount)
	}
	if got := unexpected.Load(); got != 0 {
		logFirstErrors(t, errs)
		t.Fatalf("unexpected results = %d, want 0", got)
	}

	var liveHolds, heldSeats, duplicateLive int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM holds h
		  JOIN seats s ON s.id = h.seat_id
		 WHERE s.event_id = $1::uuid AND h.status = 'active'`, eventID,
	).Scan(&liveHolds); err != nil {
		t.Fatalf("count live holds: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM seats WHERE event_id = $1::uuid AND status = 'held'`, eventID,
	).Scan(&heldSeats); err != nil {
		t.Fatalf("count held seats: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM (
		      SELECT h.seat_id
		        FROM holds h
		        JOIN seats s ON s.id = h.seat_id
		       WHERE s.event_id = $1::uuid
		         AND h.status IN ('active', 'payment_pending', 'confirmed')
		       GROUP BY h.seat_id
		      HAVING count(*) > 1
		  ) duplicates`, eventID,
	).Scan(&duplicateLive); err != nil {
		t.Fatalf("count duplicate live holds: %v", err)
	}

	if liveHolds != seatCount {
		t.Errorf("live holds = %d, want %d", liveHolds, seatCount)
	}
	if heldSeats != seatCount {
		t.Errorf("held seats = %d, want %d", heldSeats, seatCount)
	}
	if duplicateLive != 0 {
		t.Errorf("seats with multiple live holds = %d, want 0", duplicateLive)
	}

	checkInvariants(t, pool)
}

func logFirstErrors(t *testing.T, errs <-chan error) {
	t.Helper()

	logged := 0
	for err := range errs {
		if logged < 5 {
			t.Logf("unexpected error: %v", err)
			logged++
		}
	}
}
