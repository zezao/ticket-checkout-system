package redis

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

const seatMapLoadTimeout = 5 * time.Second

// Metrics records cache and Redis outcomes.
type Metrics interface {
	SeatMapCache(result string)
	RedisError()
}

// CachedSeats decorates a SeatStore with a Redis cache-aside read.
type CachedSeats struct {
	source  app.SeatStore
	cache   app.SeatMapCache
	metrics Metrics
	group   singleflight.Group
}

// NewCachedSeats builds the cache-aside seat store.
func NewCachedSeats(source app.SeatStore, cache app.SeatMapCache, metrics Metrics) *CachedSeats {
	return &CachedSeats{source: source, cache: cache, metrics: metrics}
}

// ListByEvent returns seats from Redis when possible and falls back to the source.
func (c *CachedSeats) ListByEvent(
	ctx context.Context,
	eventID string,
) ([]reservation.Seat, error) {
	seats, hit, err := c.cache.Get(ctx, eventID)
	switch {
	case err != nil:
		c.redisError()
		c.cacheResult("error")
	case hit:
		c.cacheResult("hit")
		return seats, nil
	default:
		c.cacheResult("miss")
	}

	value, err, _ := c.group.Do(eventID, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seatMapLoadTimeout)
		defer cancel()

		if seats, hit, err := c.cache.Get(loadCtx, eventID); err == nil && hit {
			c.cacheResult("hit")
			return seats, nil
		}

		seats, err := c.source.ListByEvent(loadCtx, eventID)
		if err != nil {
			return nil, err
		}
		if err := c.cache.Set(loadCtx, eventID, seats); err != nil {
			c.redisError()
			c.cacheResult("error")
		}
		return seats, nil
	})
	if err != nil {
		return nil, err
	}

	seats, ok := value.([]reservation.Seat)
	if !ok {
		return nil, fmt.Errorf("unexpected seat cache value %T", value)
	}
	return seats, nil
}

func (c *CachedSeats) cacheResult(result string) {
	if c.metrics != nil {
		c.metrics.SeatMapCache(result)
	}
}

func (c *CachedSeats) redisError() {
	if c.metrics != nil {
		c.metrics.RedisError()
	}
}
