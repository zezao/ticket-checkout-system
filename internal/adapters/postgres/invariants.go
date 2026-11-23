package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// InvariantChecker runs the data-level invariant queries against PostgreSQL.
type InvariantChecker struct {
	pool *pgxpool.Pool
}

// NewInvariantChecker builds a checker backed by pool.
func NewInvariantChecker(pool *pgxpool.Pool) *InvariantChecker {
	return &InvariantChecker{pool: pool}
}

// Check returns one violation for every invariant query that finds rows.
func (c *InvariantChecker) Check(ctx context.Context) ([]app.InvariantViolation, error) {
	checks := []struct {
		name  string
		query string
	}{
		{
			name:  "seat_missing_hold",
			query: `SELECT s.id::text FROM seats s LEFT JOIN holds h ON h.id = s.current_hold_id WHERE s.status <> 'available' AND h.id IS NULL`,
		},
		{
			name:  "sold_seat_missing_order",
			query: `SELECT s.id::text FROM seats s LEFT JOIN orders o ON o.seat_id = s.id WHERE s.status = 'sold' AND o.id IS NULL`,
		},
		{
			name:  "order_seat_not_sold",
			query: `SELECT o.id::text FROM orders o JOIN seats s ON s.id = o.seat_id WHERE s.status <> 'sold'`,
		},
		{
			name:  "held_seat_hold_not_live",
			query: `SELECT s.id::text FROM seats s JOIN holds h ON h.id = s.current_hold_id WHERE s.status = 'held' AND h.status NOT IN ('active', 'payment_pending')`,
		},
		{
			name:  "live_hold_missing_seat",
			query: `SELECT h.id::text FROM holds h LEFT JOIN seats s ON s.current_hold_id = h.id WHERE h.status IN ('active', 'payment_pending', 'confirmed') AND s.id IS NULL`,
		},
		{
			name:  "succeeded_payment_missing_order",
			query: `SELECT p.id::text FROM payment_attempts p LEFT JOIN orders o ON o.payment_attempt_id = p.id WHERE p.status = 'succeeded' AND o.id IS NULL`,
		},
		{
			name:  "order_payment_mismatch",
			query: `SELECT o.id::text FROM orders o JOIN payment_attempts p ON p.id = o.payment_attempt_id WHERE o.user_id <> p.user_id OR o.amount_cents <> p.amount_cents OR o.currency <> p.currency`,
		},
		{
			name:  "seat_hold_expiry_mismatch",
			query: `SELECT s.id::text FROM seats s JOIN holds h ON h.id = s.current_hold_id WHERE (h.status = 'active' AND s.hold_expires_at IS DISTINCT FROM h.expires_at) OR (h.status IN ('payment_pending', 'confirmed') AND s.hold_expires_at IS NOT NULL)`,
		},
	}

	violations := make([]app.InvariantViolation, 0)
	for _, check := range checks {
		ids, err := c.queryIDs(ctx, check.query)
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", check.name, err)
		}
		if len(ids) > 0 {
			violations = append(violations, app.InvariantViolation{Name: check.name, IDs: ids})
		}
	}
	return violations, nil
}

func (c *InvariantChecker) queryIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := c.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
