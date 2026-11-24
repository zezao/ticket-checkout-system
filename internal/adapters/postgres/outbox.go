package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// OutboxStore implements the transactional outbox relay reads.
type OutboxStore struct {
	pool *pgxpool.Pool
}

// NewOutboxStore builds an OutboxStore backed by pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// ClaimOutbox leases up to limit unpublished rows.
func (s *OutboxStore) ClaimOutbox(
	ctx context.Context,
	lease time.Duration,
	limit int,
) ([]app.OutboxMessage, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
		    SELECT id
		      FROM outbox
		     WHERE published_at IS NULL
		       AND (claimed_until IS NULL OR claimed_until <= clock_timestamp())
		     ORDER BY id
		     LIMIT $1
		     FOR UPDATE SKIP LOCKED
		), claimed AS (
		    UPDATE outbox o
		       SET claimed_until = clock_timestamp() + make_interval(secs => $2)
		      FROM due
		     WHERE o.id = due.id
		    RETURNING o.id, o.topic, o.payload, COALESCE(o.traceparent, '') AS traceparent
		)
		SELECT id, topic, payload, traceparent FROM claimed ORDER BY id`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	messages := make([]app.OutboxMessage, 0)
	for rows.Next() {
		var message app.OutboxMessage
		if err := rows.Scan(
			&message.ID,
			&message.Topic,
			&message.Payload,
			&message.Traceparent,
		); err != nil {
			return nil, fmt.Errorf("scan outbox message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox messages: %w", err)
	}
	return messages, nil
}

// MarkPublished acknowledges the rows whose handler succeeded.
func (s *OutboxStore) MarkPublished(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		   SET published_at = clock_timestamp(), claimed_until = NULL
		 WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return fmt.Errorf(
			"mark outbox published affected %d rows, want %d: %w",
			tag.RowsAffected(), len(ids), errConsistency)
	}
	return nil
}

// OutboxLag reports the age of the oldest unpublished row.
func (s *OutboxStore) OutboxLag(ctx context.Context) (time.Duration, error) {
	var seconds float64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(
		    EXTRACT(EPOCH FROM clock_timestamp() - min(created_at)),
		    0
		)::double precision
		  FROM outbox
		 WHERE published_at IS NULL`).Scan(&seconds); err != nil {
		return 0, fmt.Errorf("read outbox lag: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
