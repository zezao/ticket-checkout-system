package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// NewClient parses url and creates a Redis client. Connections are lazy and
// time-bounded so a missing Redis fails open quickly.
func NewClient(url string) (*goredis.Client, error) {
	options, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	options.DialTimeout = 2 * time.Second
	options.ReadTimeout = 2 * time.Second
	options.WriteTimeout = 2 * time.Second
	options.MaxRetries = 0
	return goredis.NewClient(options), nil
}

// Pinger reports whether Redis is reachable.
type Pinger struct {
	client *goredis.Client
}

// NewPinger wraps a Redis client for readiness checks.
func NewPinger(client *goredis.Client) *Pinger {
	return &Pinger{client: client}
}

// Ping checks Redis without changing any state.
func (p *Pinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}
