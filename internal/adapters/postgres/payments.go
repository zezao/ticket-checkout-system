package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/payment"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

const attemptColumns = "id::text, hold_id::text, user_id::text, idempotency_key, " +
	"amount_cents, currency, status::text, attempts, next_check_at, deadline_at, " +
	"COALESCE(last_error, ''), COALESCE(traceparent, ''), created_at, updated_at"

const attemptColumnsP = "p.id::text, p.hold_id::text, p.user_id::text, p.idempotency_key, " +
	"p.amount_cents, p.currency, p.status::text, p.attempts, p.next_check_at, p.deadline_at, " +
	"COALESCE(p.last_error, ''), COALESCE(p.traceparent, ''), p.created_at, p.updated_at"

// PaymentStore implements the payment use cases with PostgreSQL.
type PaymentStore struct {
	pool *pgxpool.Pool
}

// NewPaymentStore builds a PaymentStore backed by pool.
func NewPaymentStore(pool *pgxpool.Pool) *PaymentStore {
	return &PaymentStore{pool: pool}
}

// Start moves the hold to payment_pending and creates one attempt.
func (s *PaymentStore) Start(
	ctx context.Context,
	params app.StartPaymentParams,
) (payment.Attempt, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("begin start payment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var seatID string
	err = tx.QueryRow(ctx, `
		SELECT seat_id::text
		  FROM holds
		 WHERE id = $1::uuid
		   AND user_id = $2::uuid
		   AND status = 'active'
		   FOR UPDATE`, params.HoldID, params.UserID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.recoverStart(ctx, params)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("lock hold for payment: %w", err)
	}

	err = tx.QueryRow(ctx, `
		UPDATE holds
		   SET status = 'payment_pending', updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status = 'active'
		   AND expires_at > clock_timestamp()
		RETURNING seat_id::text`, params.HoldID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.recoverStart(ctx, params)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("start payment hold guard: %w", err)
	}

	seatUpdated, err := tx.Exec(ctx, `
		UPDATE seats
		   SET hold_expires_at = NULL
		 WHERE id = $1::uuid
		   AND current_hold_id = $2::uuid
		   AND status = 'held'`, seatID, params.HoldID)
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("clear seat expiry: %w", err)
	}
	if seatUpdated.RowsAffected() != 1 {
		return payment.Attempt{}, fmt.Errorf(
			"clear seat expiry affected %d rows: %w",
			seatUpdated.RowsAffected(), errConsistency)
	}

	attempt, err := scanAttempt(tx.QueryRow(ctx, `
		INSERT INTO payment_attempts (
		    id, hold_id, user_id, idempotency_key, amount_cents, currency,
		    deadline_at, traceparent
		)
		SELECT $1::uuid, h.id, h.user_id, $2, h.amount_cents, h.currency,
		       clock_timestamp() + make_interval(secs => $3), NULLIF($4, '')
		  FROM holds h
		 WHERE h.id = $5::uuid
		RETURNING `+attemptColumns,
		params.AttemptID,
		params.IdempotencyKey,
		params.Deadline.Seconds(),
		params.Traceparent,
		params.HoldID,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			return s.recoverStart(ctx, params)
		}
		return payment.Attempt{}, fmt.Errorf("insert payment attempt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return payment.Attempt{}, fmt.Errorf("commit start payment: %w", err)
	}
	return attempt, nil
}

// GetForHold returns the caller's attempt for a hold, if one exists.
func (s *PaymentStore) GetForHold(
	ctx context.Context,
	holdID, userID string,
) (payment.Attempt, bool, error) {
	attempt, err := scanAttempt(s.pool.QueryRow(ctx, `
		SELECT `+attemptColumnsP+`
		  FROM payment_attempts p
		  JOIN holds h ON h.id = p.hold_id
		 WHERE p.hold_id = $1::uuid
		   AND h.user_id = $2::uuid`, holdID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, false, nil
	}
	if err != nil {
		return payment.Attempt{}, false, fmt.Errorf("get attempt: %w", err)
	}
	return attempt, true, nil
}

// ClaimDue leases due attempts to this worker.
func (s *PaymentStore) ClaimDue(
	ctx context.Context,
	lease time.Duration,
	limit int,
) ([]payment.Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
		    SELECT id
		      FROM payment_attempts
		     WHERE status IN ('created', 'pending')
		       AND next_check_at <= clock_timestamp()
		     ORDER BY id
		     LIMIT $1
		     FOR UPDATE SKIP LOCKED
		), claimed AS (
		    UPDATE payment_attempts p
		       SET next_check_at = clock_timestamp() + make_interval(secs => $2),
		           attempts = p.attempts + 1,
		           updated_at = clock_timestamp()
		      FROM due
		     WHERE p.id = due.id
		    RETURNING `+attemptColumnsP+`
		)
		SELECT * FROM claimed ORDER BY 1`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim due payments: %w", err)
	}
	defer rows.Close()

	attempts := make([]payment.Attempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed payment: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed payments: %w", err)
	}
	return attempts, nil
}

// ApplyResult applies one provider result in a guarded transaction.
func (s *PaymentStore) ApplyResult(
	ctx context.Context,
	params app.SettleParams,
) (app.SettleResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.SettleResult{}, fmt.Errorf("begin settlement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	attempt, err := scanAttempt(tx.QueryRow(ctx,
		"SELECT "+attemptColumns+" FROM payment_attempts WHERE id = $1::uuid FOR UPDATE",
		params.AttemptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.SettleResult{}, app.ErrNotFound
	}
	if err != nil {
		return app.SettleResult{}, fmt.Errorf("lock payment attempt: %w", err)
	}
	if !attempt.Open() {
		_ = tx.Rollback(ctx)
		return app.SettleResult{Applied: false, Attempt: attempt}, nil
	}

	var updated payment.Attempt
	switch params.Outcome {
	case app.SettlementPending:
		updated, err = applyPending(ctx, tx, params)
	case app.SettlementSucceeded:
		updated, err = applySucceeded(ctx, tx, attempt)
	case app.SettlementDeclined:
		updated, err = applyDeclined(ctx, tx, attempt)
	default:
		return app.SettleResult{}, fmt.Errorf("unknown settlement outcome %q", params.Outcome)
	}
	if err != nil {
		return app.SettleResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return app.SettleResult{}, fmt.Errorf("commit settlement: %w", err)
	}
	return app.SettleResult{Applied: true, Attempt: updated}, nil
}

// CountOverdue reports attempts that are still open past their deadline.
func (s *PaymentStore) CountOverdue(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM payment_attempts
		 WHERE status IN ('created', 'pending')
		   AND deadline_at <= clock_timestamp()`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count overdue payments: %w", err)
	}
	return count, nil
}

func applyPending(
	ctx context.Context,
	tx pgx.Tx,
	params app.SettleParams,
) (payment.Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx, `
		UPDATE payment_attempts
		   SET status = 'pending',
		       last_error = NULLIF($2, ''),
		       next_check_at = clock_timestamp() + make_interval(secs => $3),
		       updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status IN ('created', 'pending')
		RETURNING `+attemptColumns,
		params.AttemptID, params.LastError, params.Backoff.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("pending update matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("update attempt to pending: %w", err)
	}
	return attempt, nil
}

func applySucceeded(
	ctx context.Context,
	tx pgx.Tx,
	attempt payment.Attempt,
) (payment.Attempt, error) {
	updated, err := scanAttempt(tx.QueryRow(ctx, `
		UPDATE payment_attempts
		   SET status = 'succeeded', last_error = NULL, updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status IN ('created', 'pending')
		RETURNING `+attemptColumns, attempt.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("succeeded update matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("update attempt to succeeded: %w", err)
	}

	var seatID string
	err = tx.QueryRow(ctx, `
		UPDATE holds
		   SET status = 'confirmed', updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status = 'payment_pending'
		RETURNING seat_id::text`, attempt.HoldID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("confirm hold matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("confirm hold: %w", err)
	}

	var eventID string
	err = tx.QueryRow(ctx, `
		UPDATE seats
		   SET status = 'sold', hold_expires_at = NULL
		 WHERE id = $1::uuid
		   AND current_hold_id = $2::uuid
		   AND status = 'held'
		RETURNING event_id::text`, seatID, attempt.HoldID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("sell seat matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("sell seat: %w", err)
	}

	var orderID string
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (hold_id, seat_id, payment_attempt_id, user_id, amount_cents, currency)
		SELECT p.hold_id, h.seat_id, p.id, p.user_id, p.amount_cents, p.currency
		  FROM payment_attempts p
		  JOIN holds h ON h.id = p.hold_id
		 WHERE p.id = $1::uuid
		RETURNING id::text`, attempt.ID).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("insert order matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("insert order: %w", err)
	}

	outbox, err := tx.Exec(ctx, `
		INSERT INTO outbox (topic, payload, traceparent) VALUES
		    ('seat.changed', jsonb_build_object('event_id', $1::uuid, 'seat_id', $2::uuid),
		     NULLIF($5, '')),
		    ('order.confirmed', jsonb_build_object('order_id', $3::uuid, 'hold_id', $4::uuid, 'seat_id', $2::uuid),
		     NULLIF($5, ''))`,
		eventID, seatID, orderID, attempt.HoldID, observability.Traceparent(ctx))
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("insert settlement outbox: %w", err)
	}
	if outbox.RowsAffected() != 2 {
		return payment.Attempt{}, fmt.Errorf(
			"insert settlement outbox affected %d rows: %w",
			outbox.RowsAffected(), errConsistency)
	}

	return updated, nil
}

func applyDeclined(
	ctx context.Context,
	tx pgx.Tx,
	attempt payment.Attempt,
) (payment.Attempt, error) {
	updated, err := scanAttempt(tx.QueryRow(ctx, `
		UPDATE payment_attempts
		   SET status = 'failed', last_error = NULL, updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status IN ('created', 'pending')
		RETURNING `+attemptColumns, attempt.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("failed update matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("update attempt to failed: %w", err)
	}

	var seatID string
	err = tx.QueryRow(ctx, `
		UPDATE holds
		   SET status = 'payment_failed', updated_at = clock_timestamp()
		 WHERE id = $1::uuid
		   AND status = 'payment_pending'
		RETURNING seat_id::text`, attempt.HoldID).Scan(&seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("fail hold matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("fail hold: %w", err)
	}

	var eventID string
	err = tx.QueryRow(ctx, `
		UPDATE seats
		   SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
		 WHERE id = $1::uuid
		   AND current_hold_id = $2::uuid
		   AND status = 'held'
		RETURNING event_id::text`, seatID, attempt.HoldID).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Attempt{}, fmt.Errorf("release seat matched no rows: %w", errConsistency)
	}
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("release seat: %w", err)
	}

	outbox, err := tx.Exec(ctx, `
		INSERT INTO outbox (topic, payload, traceparent)
		VALUES ('seat.changed', jsonb_build_object('event_id', $1::uuid, 'seat_id', $2::uuid),
		        NULLIF($3, ''))`,
		eventID, seatID, observability.Traceparent(ctx))
	if err != nil {
		return payment.Attempt{}, fmt.Errorf("insert decline outbox: %w", err)
	}
	if outbox.RowsAffected() != 1 {
		return payment.Attempt{}, fmt.Errorf(
			"insert decline outbox affected %d rows: %w",
			outbox.RowsAffected(), errConsistency)
	}

	return updated, nil
}

func (s *PaymentStore) recoverStart(
	ctx context.Context,
	params app.StartPaymentParams,
) (payment.Attempt, error) {
	attempt, err := scanAttempt(s.pool.QueryRow(ctx,
		"SELECT "+attemptColumns+" FROM payment_attempts WHERE user_id = $1::uuid AND idempotency_key = $2",
		params.UserID, params.IdempotencyKey))
	switch {
	case err == nil:
		if attempt.HoldID == params.HoldID {
			return attempt, nil
		}
		return payment.Attempt{}, app.ErrIdempotencyKeyReused
	case !errors.Is(err, pgx.ErrNoRows):
		return payment.Attempt{}, fmt.Errorf("find attempt by key: %w", err)
	}

	attempt, err = scanAttempt(s.pool.QueryRow(ctx,
		"SELECT "+attemptColumns+" FROM payment_attempts WHERE hold_id = $1::uuid", params.HoldID))
	switch {
	case err == nil:
		if attempt.Open() {
			return payment.Attempt{}, app.ErrPaymentInProgress
		}
		return payment.Attempt{}, app.ErrHoldNotActive
	case !errors.Is(err, pgx.ErrNoRows):
		return payment.Attempt{}, fmt.Errorf("find attempt by hold: %w", err)
	}

	var ownerID string
	err = s.pool.QueryRow(ctx,
		"SELECT user_id::text FROM holds WHERE id = $1::uuid", params.HoldID).Scan(&ownerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return payment.Attempt{}, app.ErrNotFound
	case err != nil:
		return payment.Attempt{}, fmt.Errorf("find hold: %w", err)
	}
	if ownerID != params.UserID {
		return payment.Attempt{}, app.ErrNotFound
	}
	return payment.Attempt{}, app.ErrHoldNotActive
}

func scanAttempt(row rowScanner) (payment.Attempt, error) {
	var (
		attempt payment.Attempt
		status  string
	)
	if err := row.Scan(
		&attempt.ID,
		&attempt.HoldID,
		&attempt.UserID,
		&attempt.IdempotencyKey,
		&attempt.AmountCents,
		&attempt.Currency,
		&status,
		&attempt.Attempts,
		&attempt.NextCheckAt,
		&attempt.DeadlineAt,
		&attempt.LastError,
		&attempt.Traceparent,
		&attempt.CreatedAt,
		&attempt.UpdatedAt,
	); err != nil {
		return payment.Attempt{}, err
	}

	attempt.Status = payment.Status(status)
	attempt.NextCheckAt = attempt.NextCheckAt.UTC()
	attempt.DeadlineAt = attempt.DeadlineAt.UTC()
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.UpdatedAt = attempt.UpdatedAt.UTC()
	return attempt, nil
}
