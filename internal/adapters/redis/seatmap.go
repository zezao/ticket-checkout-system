package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const seatMapTTL = 5 * time.Second

// SeatMapCache stores a compact seat map per event in Redis.
type SeatMapCache struct {
	client *goredis.Client
}

// NewSeatMapCache builds a Redis-backed seat-map cache.
func NewSeatMapCache(client *goredis.Client) *SeatMapCache {
	return &SeatMapCache{client: client}
}

// Get returns the cached seats, or false on a miss.
func (c *SeatMapCache) Get(
	ctx context.Context,
	eventID string,
) ([]reservation.Seat, bool, error) {
	data, err := c.client.Get(ctx, seatMapKey(eventID)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis seat map get: %w", err)
	}

	var entries []cachedSeat
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false, fmt.Errorf("decode seat map cache: %w", err)
	}

	seats := make([]reservation.Seat, len(entries))
	for i, entry := range entries {
		seats[i] = reservation.Seat{
			ID:         entry.ID,
			EventID:    eventID,
			Section:    entry.Section,
			RowLabel:   entry.Row,
			Number:     entry.Number,
			PriceCents: entry.PriceCents,
			Status:     reservation.SeatStatus(entry.Status),
		}
	}
	return seats, true, nil
}

// Set replaces the cached seats for an event.
func (c *SeatMapCache) Set(
	ctx context.Context,
	eventID string,
	seats []reservation.Seat,
) error {
	entries := make([]cachedSeat, len(seats))
	for i, seat := range seats {
		entries[i] = cachedSeat{
			ID:         seat.ID,
			Section:    seat.Section,
			Row:        seat.RowLabel,
			Number:     seat.Number,
			Status:     string(seat.Status),
			PriceCents: seat.PriceCents,
		}
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode seat map cache: %w", err)
	}
	if err := c.client.Set(ctx, seatMapKey(eventID), data, seatMapTTL).Err(); err != nil {
		return fmt.Errorf("redis seat map set: %w", err)
	}
	return nil
}

// Delete invalidates the cached seats for an event.
func (c *SeatMapCache) Delete(ctx context.Context, eventID string) error {
	if err := c.client.Del(ctx, seatMapKey(eventID)).Err(); err != nil {
		return fmt.Errorf("redis seat map delete: %w", err)
	}
	return nil
}

func seatMapKey(eventID string) string {
	return "seatmap:" + eventID
}

type cachedSeat struct {
	ID         string `json:"id"`
	Section    string `json:"section"`
	Row        string `json:"row"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	PriceCents int64  `json:"price_cents"`
}
