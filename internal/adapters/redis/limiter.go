package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const rateLimitWindow = 2 * time.Minute

var rateLimitScript = goredis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// Limiter enforces a fixed-window per-user hold limit.
type Limiter struct {
	client  *goredis.Client
	limit   int
	metrics Metrics
}

// NewLimiter builds a fixed-window rate limiter.
func NewLimiter(client *goredis.Client, limit int, metrics Metrics) *Limiter {
	return &Limiter{client: client, limit: limit, metrics: metrics}
}

// Allow reports whether the user may make another hold request.
func (l *Limiter) Allow(ctx context.Context, userID string) (bool, error) {
	window := time.Now().Unix() / 60
	key := fmt.Sprintf("ratelimit:holds:%s:%d", userID, window)

	count, err := rateLimitScript.Run(ctx, l.client, []string{key},
		int(rateLimitWindow.Seconds())).Int64()
	if err != nil {
		if l.metrics != nil {
			l.metrics.RedisError()
		}
		return false, fmt.Errorf("redis rate limit: %w", err)
	}
	return count <= int64(l.limit), nil
}
