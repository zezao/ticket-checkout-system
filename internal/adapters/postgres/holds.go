package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const holdColumns = "id::text, seat_id::text, user_id::text, status::text, " +
	"expires_at, amount_cents, currency, created_at, updated_at"

var errConsistency = errors.New("consistency error")

// HoldStore implements the hold lifecycle with PostgreSQL as the arbiter.
type HoldStore struct {
	pool *pgxpool.Pool
}

// NewHoldStore builds a HoldStore backed by pool.
func NewHoldStore(pool *pgxpool.Pool) *HoldStore {
	return &HoldStore{pool: pool}
}

// Acquire holds a seat or returns the caller's existing live hold.
func (s *HoldStore) Acquire(ctx context.Context, params app.AcquireParams) (app.AcquireResult, error) {
	hold, ok, err := s.liveHoldForUser(ctx, params.SeatID, params.UserID)
	if err != nil {
		return app.AcquireResult{}, err
	}
	if ok {
		return app.AcquireResult{Hold: hold}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.AcquireResult{}, fmt.Errorf("begin acquire: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE holds
		   SET status = 'expired', updated_at = clock_timestamp()
		 WHERE seat_id = $1::uuid
		   AND status = 'active'
		   AND expires_at <= clock_timestamp()`, params.SeatID); err != nil {
		return app.AcquireResult{}, fmt.Errorf("retire expired hold: %w", err)
	}

	var (
		amountCents int64
		currency    string
		expiresAt   time.Time
		eventID     string
		seatID      string
	)
	err = tx.QueryRow(ctx, `
		UPDATE seats s
		   SET status = 'held',
		       current_hold_id = $1::uuid,
		       hold_expires_at = clock_timestamp() + make_interval(secs => $2)
		  FROM events e
		 WHERE s.id = $3::uuid
		   AND s.event_id = $4::uuid
		   AND e.id = s.event_id
		   AND (s.status = 'available'
		        OR (s.status = 'held' AND s.hold_expires_at <= clock_timestamp()))
		RETURNING s.price_cents, e.currency, s.hold_expires_at, e.id::text, s.id::text`,
		params.HoldID, params.TTL.Seconds(), params.SeatID, params.EventID,
	).Scan(&amountCents, &currency, &expiresAt, &eventID, &seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.recoverAcquire(ctx, params)
	}
	if err != nil {
		return app.AcquireResult{}, fmt.Errorf("claim seat: %w", err)
	}

	inserted, err := tx.Exec(ctx, `
		INSERT INTO holds (id, seat_id, user_id, status, expires_at, amount_cents, currency)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'active', $4, $5, $6)`,
		params.HoldID, seatID, params.UserID, expiresAt, amountCents, currency)
	if err != nil {
		return app.AcquireResult{}, fmt.Errorf("insert hold: %w", err)
	}
	if inserted.RowsAffected() != 1 {
		return app.AcquireResult{}, fmt.Errorf(
			"insert hold affected %d rows: %w", inserted.RowsAffected(), errConsistency)
	}

	evented, err := tx.Exec(ctx, `
		INSERT INTO outbox (topic, payload)
		VALUES ('seat.changed', jsonb_build_object('event_id', $1::uuid, 'seat_id', $2::uuid))`,
		eventID, seatID)
	if err != nil {
		return app.AcquireResult{}, fmt.Errorf("insert seat.changed outbox: %w", err)
	}
	if evented.RowsAffected() != 1 {
		return app.AcquireResult{}, fmt.Errorf(
			"insert seat.changed affected %d rows: %w", evented.RowsAffected(), errConsistency)
	}

	if err := tx.Commit(ctx); err != nil {
		return app.AcquireResult{}, fmt.Errorf("commit acquire: %w", err)
	}

	return app.AcquireResult{
		Hold: reservation.Hold{
			ID:          params.HoldID,
			SeatID:      seatID,
			UserID:      params.UserID,
			Status:      reservation.HoldActive,
			ExpiresAt:   expiresAt.UTC(),
			AmountCents: amountCents,
			Currency:    currency,
		},
		Created: true,
	}, nil
}

// Release gives an active hold back to the pool.
func (s *HoldStore) Release(ctx context.Context, holdID, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query, _, err := holdTransitionSQL(reservation.HoldReleased)
	if err != nil {
		return err
	}

	var seatID string
	err = tx.QueryRow(ctx, query+" RETURNING seat_id::text", holdID, userID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.releaseFailure(ctx, holdID, userID)
	}
	if err != nil {
		return fmt.Errorf("release hold: %w", err)
	}

	var eventID string
	err = tx.QueryRow(ctx, `
		UPDATE seats
		   SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
		 WHERE id = $1::uuid
		   AND current_hold_id = $2::uuid
		   AND status = 'held'
		RETURNING event_id::text`, seatID, holdID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("release hold %s: seat %s did not point back: %w", holdID, seatID, errConsistency)
	}
	if err != nil {
		return fmt.Errorf("release seat: %w", err)
	}

	evented, err := tx.Exec(ctx, `
		INSERT INTO outbox (topic, payload)
		VALUES ('seat.changed', jsonb_build_object('event_id', $1::uuid, 'seat_id', $2::uuid))`,
		eventID, seatID)
	if err != nil {
		return fmt.Errorf("insert seat.changed outbox: %w", err)
	}
	if evented.RowsAffected() != 1 {
		return fmt.Errorf(
			"insert seat.changed affected %d rows: %w", evented.RowsAffected(), errConsistency)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release: %w", err)
	}
	return nil
}

// Get returns one of the caller's holds.
func (s *HoldStore) Get(ctx context.Context, holdID, userID string) (reservation.Hold, error) {
	hold, err := scanHold(s.pool.QueryRow(ctx,
		"SELECT "+holdColumns+" FROM holds WHERE id = $1::uuid AND user_id = $2::uuid",
		holdID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation.Hold{}, app.ErrNotFound
	}
	if err != nil {
		return reservation.Hold{}, fmt.Errorf("get hold: %w", err)
	}
	return hold, nil
}

// ExpireBatch retires up to limit expired active holds and frees their seats.
func (s *HoldStore) ExpireBatch(ctx context.Context, limit int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin expiry sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var updatedHolds, updatedSeats, written int64
	err = tx.QueryRow(ctx, `
		WITH expired_holds AS (
		    SELECT id, seat_id
		      FROM holds
		     WHERE status = 'active'
		       AND expires_at <= clock_timestamp()
		     ORDER BY id
		     LIMIT $1
		     FOR UPDATE SKIP LOCKED
		), updated_holds AS (
		    UPDATE holds h
		       SET status = 'expired', updated_at = clock_timestamp()
		      FROM expired_holds eh
		     WHERE h.id = eh.id
		    RETURNING h.id, h.seat_id
		), updated_seats AS (
		    UPDATE seats s
		       SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
		      FROM updated_holds uh
		     WHERE s.id = uh.seat_id
		       AND s.current_hold_id = uh.id
		       AND s.status = 'held'
		    RETURNING s.id, s.event_id
		), written AS (
		    INSERT INTO outbox (topic, payload)
		    SELECT 'seat.changed', jsonb_build_object('event_id', us.event_id, 'seat_id', us.id)
		      FROM updated_seats us
		    RETURNING 1
		)
		SELECT (SELECT count(*) FROM updated_holds),
		       (SELECT count(*) FROM updated_seats),
		       (SELECT count(*) FROM written)`, limit,
	).Scan(&updatedHolds, &updatedSeats, &written)
	if err != nil {
		return 0, fmt.Errorf("expire holds: %w", err)
	}
	if updatedHolds != updatedSeats || updatedHolds != written {
		return 0, fmt.Errorf(
			"expire holds: holds=%d seats=%d outbox=%d: %w",
			updatedHolds, updatedSeats, written, errConsistency)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expiry sweep: %w", err)
	}
	return int(updatedHolds), nil
}

// TryTransition applies the same status guard as the write paths inside a
// transaction that is always rolled back. It exists so the domain and SQL
// agreement test can compare both decisions.
func (s *HoldStore) TryTransition(
	ctx context.Context,
	holdID, userID string,
	to reservation.HoldStatus,
) (bool, error) {
	query, ownerScoped, err := holdTransitionSQL(to)
	if err != nil {
		return false, err
	}
	if query == "" {
		return false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin transition attempt: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	args := []any{holdID}
	if ownerScoped {
		args = append(args, userID)
	}

	affected, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("try transition: %w", err)
	}
	return affected.RowsAffected() == 1, nil
}

func (s *HoldStore) recoverAcquire(
	ctx context.Context,
	params app.AcquireParams,
) (app.AcquireResult, error) {
	hold, ok, err := s.liveHoldForUser(ctx, params.SeatID, params.UserID)
	if err != nil {
		return app.AcquireResult{}, err
	}
	if ok {
		return app.AcquireResult{Hold: hold}, nil
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM seats WHERE id = $1::uuid AND event_id = $2::uuid
		)`, params.SeatID, params.EventID).Scan(&exists); err != nil {
		return app.AcquireResult{}, fmt.Errorf("check seat: %w", err)
	}
	if !exists {
		return app.AcquireResult{}, app.ErrNotFound
	}
	return app.AcquireResult{}, app.ErrSeatUnavailable
}

func (s *HoldStore) liveHoldForUser(
	ctx context.Context,
	seatID, userID string,
) (reservation.Hold, bool, error) {
	hold, err := scanHold(s.pool.QueryRow(ctx, `
		SELECT `+holdColumns+`
		  FROM holds
		 WHERE seat_id = $1::uuid
		   AND user_id = $2::uuid
		   AND ((status = 'active' AND expires_at > clock_timestamp())
		        OR status = 'payment_pending')
		 ORDER BY created_at DESC
		 LIMIT 1`, seatID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation.Hold{}, false, nil
	}
	if err != nil {
		return reservation.Hold{}, false, fmt.Errorf("find live hold: %w", err)
	}
	return hold, true, nil
}

func (s *HoldStore) releaseFailure(ctx context.Context, holdID, userID string) error {
	var ownerID, status string
	err := s.pool.QueryRow(ctx,
		"SELECT user_id::text, status::text FROM holds WHERE id = $1::uuid", holdID,
	).Scan(&ownerID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read hold after release miss: %w", err)
	}
	if ownerID != userID {
		return app.ErrNotFound
	}
	return app.ErrHoldNotActive
}

func scanHold(row rowScanner) (reservation.Hold, error) {
	var (
		hold   reservation.Hold
		status string
	)
	if err := row.Scan(
		&hold.ID,
		&hold.SeatID,
		&hold.UserID,
		&status,
		&hold.ExpiresAt,
		&hold.AmountCents,
		&hold.Currency,
		&hold.CreatedAt,
		&hold.UpdatedAt,
	); err != nil {
		return reservation.Hold{}, err
	}
	hold.Status = reservation.HoldStatus(status)
	hold.ExpiresAt = hold.ExpiresAt.UTC()
	hold.CreatedAt = hold.CreatedAt.UTC()
	hold.UpdatedAt = hold.UpdatedAt.UTC()
	return hold, nil
}

func holdTransitionSQL(to reservation.HoldStatus) (query string, ownerScoped bool, err error) {
	switch to {
	case reservation.HoldActive:
		return "", false, nil
	case reservation.HoldPaymentPending:
		return `
			UPDATE holds
			   SET status = 'payment_pending', updated_at = clock_timestamp()
			 WHERE id = $1::uuid
			   AND user_id = $2::uuid
			   AND status = 'active'
			   AND expires_at > clock_timestamp()`, true, nil
	case reservation.HoldConfirmed:
		return `
			UPDATE holds
			   SET status = 'confirmed', updated_at = clock_timestamp()
			 WHERE id = $1::uuid
			   AND status = 'payment_pending'`, false, nil
	case reservation.HoldExpired:
		return `
			UPDATE holds
			   SET status = 'expired', updated_at = clock_timestamp()
			 WHERE id = $1::uuid
			   AND status = 'active'
			   AND expires_at <= clock_timestamp()`, false, nil
	case reservation.HoldReleased:
		return `
			UPDATE holds
			   SET status = 'released', updated_at = clock_timestamp()
			 WHERE id = $1::uuid
			   AND user_id = $2::uuid
			   AND status = 'active'`, true, nil
	case reservation.HoldPaymentFailed:
		return `
			UPDATE holds
			   SET status = 'payment_failed', updated_at = clock_timestamp()
			 WHERE id = $1::uuid
			   AND status = 'payment_pending'`, false, nil
	default:
		return "", false, fmt.Errorf("unknown hold status %q", to)
	}
}
