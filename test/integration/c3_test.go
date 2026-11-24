package integration_test

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/worker"
)

func TestC3_RandomizedMix(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newPool(t)
	eventID := insertEvent(t, pool)

	const (
		seatCount = 10
		userCount = 20
		rounds    = 300
	)

	seatIDs := make([]string, seatCount)
	for i := range seatIDs {
		seatIDs[i] = insertSeat(t, pool, eventID, "GA", "A", i+1, 8500)
	}
	userIDs := make([]string, userCount)
	for i := range userIDs {
		userIDs[i] = uuid.NewString()
	}

	holdStore := pgadapter.NewHoldStore(pool)
	paymentStore := pgadapter.NewPaymentStore(pool)
	reservations := app.NewReservation(holdStore, newHoldID, 2*time.Second)
	payments := app.NewPayments(paymentStore, newHoldID, 5*time.Minute)
	provider := newMockProvider(t, "success")
	settlement := app.NewSettlement(paymentStore, provider, 2*time.Second)

	startedAt := time.Now().Add(-time.Second)

	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		worker.NewSettler(paymentStore, settlement, discardLogger(),
			300*time.Millisecond, 100*time.Millisecond, nil).Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		worker.NewSweeper(holdStore, discardLogger(), 100*time.Millisecond, nil).Run(runCtx)
	}()

	type trackedHold struct {
		holdID string
		userID string
	}
	tracked := make([]trackedHold, 0, rounds)
	rng := rand.New(rand.NewSource(42))

	for i := 0; i < rounds; i++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4:
			userID := userIDs[rng.Intn(len(userIDs))]
			seatID := seatIDs[rng.Intn(len(seatIDs))]

			result, err := reservations.Acquire(ctx, app.AcquireRequest{
				EventID: eventID,
				SeatID:  seatID,
				UserID:  userID,
			})
			switch {
			case err == nil:
				tracked = append(tracked, trackedHold{holdID: result.Hold.ID, userID: userID})
				if rng.Intn(2) == 0 {
					_, _ = payments.Start(ctx, app.StartPaymentRequest{
						HoldID:         result.Hold.ID,
						UserID:         userID,
						IdempotencyKey: uuid.NewString(),
					})
				}
			case errors.Is(err, app.ErrSeatUnavailable):
			default:
				t.Fatalf("acquire: %v", err)
			}

		case 5, 6:
			if len(tracked) == 0 {
				continue
			}
			hold := tracked[rng.Intn(len(tracked))]
			err := reservations.Release(ctx, hold.holdID, hold.userID)
			if err != nil &&
				!errors.Is(err, app.ErrNotFound) &&
				!errors.Is(err, app.ErrHoldNotActive) {
				t.Fatalf("release: %v", err)
			}

		case 7, 8:
			if len(tracked) == 0 {
				continue
			}
			hold := tracked[rng.Intn(len(tracked))]
			_, err := payments.Start(ctx, app.StartPaymentRequest{
				HoldID:         hold.holdID,
				UserID:         hold.userID,
				IdempotencyKey: uuid.NewString(),
			})
			if err != nil &&
				!errors.Is(err, app.ErrNotFound) &&
				!errors.Is(err, app.ErrHoldNotActive) &&
				!errors.Is(err, app.ErrPaymentInProgress) &&
				!errors.Is(err, app.ErrIdempotencyKeyReused) {
				t.Fatalf("start payment: %v", err)
			}

		case 9:
			time.Sleep(10 * time.Millisecond)
		}

		time.Sleep(time.Duration(rng.Intn(4)) * time.Millisecond)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		var open int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM payment_attempts
			 WHERE status IN ('created', 'pending')
			   AND created_at >= $1`, startedAt,
		).Scan(&open); err != nil {
			t.Fatalf("count open attempts: %v", err)
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("payments did not settle; open = %d", open)
		}
		time.Sleep(100 * time.Millisecond)
	}

	stop()
	wg.Wait()

	if _, err := holdStore.ExpireBatch(ctx, 1000); err != nil {
		t.Fatalf("final sweep: %v", err)
	}

	checkInvariants(t, pool)
}
