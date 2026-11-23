package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

func TestHoldTransitionAgreement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	eventID := insertEvent(t, pool)
	store := pgadapter.NewHoldStore(pool)
	owner := uuid.NewString()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1::uuid", eventID)
	})

	statuses := []reservation.HoldStatus{
		reservation.HoldActive,
		reservation.HoldPaymentPending,
		reservation.HoldConfirmed,
		reservation.HoldExpired,
		reservation.HoldReleased,
		reservation.HoldPaymentFailed,
	}

	seatNumber := 1
	for _, from := range statuses {
		for _, to := range statuses {
			from, to := from, to
			number := seatNumber
			seatNumber++

			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				seatID := insertSeat(t, pool, eventID, "GA", "A", number, 8500)
				holdID := uuid.NewString()
				expiresAt := time.Now().Add(10 * time.Minute).UTC()
				if from == reservation.HoldActive && to == reservation.HoldExpired {
					expiresAt = time.Now().Add(-time.Minute).UTC()
				}

				insertHold(t, pool, holdID, seatID, owner, from, expiresAt)
				t.Cleanup(func() {
					_, _ = pool.Exec(context.Background(),
						"DELETE FROM holds WHERE id = $1::uuid", holdID)
					_, _ = pool.Exec(context.Background(),
						"DELETE FROM seats WHERE id = $1::uuid", seatID)
				})

				accepted, err := store.TryTransition(ctx, holdID, owner, to)
				if err != nil {
					t.Fatalf("TryTransition: %v", err)
				}

				want := reservation.CanTransition(from, to) == nil
				if accepted != want {
					t.Errorf("from=%s to=%s: sql accepted=%t, domain accepted=%t",
						from, to, accepted, want)
				}
			})
		}
	}

	checkInvariants(t, pool)
}
