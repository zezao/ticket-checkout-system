package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

func TestInvariantCheckerDetectsMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	userID := uuid.NewString()
	holdID := uuid.NewString()
	future := time.Now().Add(10 * time.Minute).UTC()

	insertHold(t, pool, holdID, seatID, userID, reservation.HoldActive, future)
	holdSeat(t, pool, seatID, holdID, &future)

	if _, err := pool.Exec(ctx, `
		UPDATE holds
		   SET expires_at = expires_at + interval '1 second'
		 WHERE id = $1::uuid`, holdID,
	); err != nil {
		t.Fatalf("corrupt hold expiry: %v", err)
	}

	checker := pgadapter.NewInvariantChecker(pool)
	violations, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	found := false
	for _, violation := range violations {
		if violation.Name != "seat_hold_expiry_mismatch" {
			continue
		}
		for _, id := range violation.IDs {
			if id == seatID {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("checker did not report seat %s: %+v", seatID, violations)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET hold_expires_at = (SELECT expires_at FROM holds WHERE id = $1::uuid)
		 WHERE id = $2::uuid`, holdID, seatID,
	); err != nil {
		t.Fatalf("repair seat expiry: %v", err)
	}

	checkInvariants(t, pool)
}
