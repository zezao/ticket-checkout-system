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

const eventColumns = "id::text, name, venue, starts_at, currency"

// Catalog implements the read-side stores with PostgreSQL.
type Catalog struct {
	pool *pgxpool.Pool
}

// NewCatalog builds a Catalog backed by pool.
func NewCatalog(pool *pgxpool.Pool) *Catalog {
	return &Catalog{pool: pool}
}

// ListEvents returns every event ordered by start time.
func (c *Catalog) ListEvents(ctx context.Context) ([]app.Event, error) {
	rows, err := c.pool.Query(ctx,
		"SELECT "+eventColumns+" FROM events ORDER BY starts_at, id")
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	events := make([]app.Event, 0)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

// GetEvent returns one event or app.ErrNotFound.
func (c *Catalog) GetEvent(ctx context.Context, id string) (app.Event, error) {
	row := c.pool.QueryRow(ctx,
		"SELECT "+eventColumns+" FROM events WHERE id = $1::uuid", id)

	event, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Event{}, app.ErrNotFound
	}
	if err != nil {
		return app.Event{}, fmt.Errorf("scan event: %w", err)
	}
	return event, nil
}

// ListByEvent returns the seats that belong to an event.
func (c *Catalog) ListByEvent(ctx context.Context, eventID string) ([]reservation.Seat, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT id::text, event_id::text, section, row_label, number, price_cents, status::text
		  FROM seats
		 WHERE event_id = $1::uuid
		 ORDER BY section, row_label, number, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("query seats: %w", err)
	}
	defer rows.Close()

	seats := make([]reservation.Seat, 0)
	for rows.Next() {
		var (
			seat   reservation.Seat
			status string
		)
		if err := rows.Scan(
			&seat.ID,
			&seat.EventID,
			&seat.Section,
			&seat.RowLabel,
			&seat.Number,
			&seat.PriceCents,
			&status,
		); err != nil {
			return nil, fmt.Errorf("scan seat: %w", err)
		}
		seat.Status = reservation.SeatStatus(status)
		seats = append(seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seats: %w", err)
	}
	return seats, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (app.Event, error) {
	var (
		event    app.Event
		startsAt time.Time
	)
	if err := row.Scan(&event.ID, &event.Name, &event.Venue, &startsAt, &event.Currency); err != nil {
		return app.Event{}, err
	}
	event.StartsAt = startsAt.UTC()
	return event, nil
}
