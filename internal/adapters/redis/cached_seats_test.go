package redis

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type fakeSeatSource struct {
	mu      sync.Mutex
	seats   []reservation.Seat
	err     error
	calls   int
	started chan struct{}
	release chan struct{}
}

func (s *fakeSeatSource) ListByEvent(context.Context, string) ([]reservation.Seat, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	return s.seats, s.err
}

func (s *fakeSeatSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fakeSeatCache struct {
	mu      sync.Mutex
	seats   []reservation.Seat
	hit     bool
	getErr  error
	setErr  error
	sets    int
	deletes int
}

func (c *fakeSeatCache) Get(context.Context, string) ([]reservation.Seat, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seats, c.hit, c.getErr
}

func (c *fakeSeatCache) Set(_ context.Context, _ string, seats []reservation.Seat) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	c.seats = seats
	c.hit = true
	return c.setErr
}

func (c *fakeSeatCache) Delete(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	c.hit = false
	return nil
}

type fakeCacheMetrics struct {
	hits, misses, errors, redisErrors int
}

func (m *fakeCacheMetrics) SeatMapCache(result string) {
	switch result {
	case "hit":
		m.hits++
	case "miss":
		m.misses++
	case "error":
		m.errors++
	}
}

func (m *fakeCacheMetrics) RedisError() {
	m.redisErrors++
}

func TestCachedSeatsHit(t *testing.T) {
	t.Parallel()

	source := &fakeSeatSource{seats: []reservation.Seat{{ID: "from-source"}}}
	cache := &fakeSeatCache{seats: []reservation.Seat{{ID: "from-cache"}}, hit: true}
	metrics := &fakeCacheMetrics{}
	seats := NewCachedSeats(source, cache, metrics)

	got, err := seats.ListByEvent(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("ListByEvent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "from-cache" {
		t.Errorf("seats = %+v", got)
	}
	if got := source.callCount(); got != 0 {
		t.Errorf("source calls = %d, want 0", got)
	}
	if metrics.hits != 1 {
		t.Errorf("hits = %d, want 1", metrics.hits)
	}
}

func TestCachedSeatsMissPopulates(t *testing.T) {
	t.Parallel()

	source := &fakeSeatSource{seats: []reservation.Seat{{ID: "seat-1"}}}
	cache := &fakeSeatCache{}
	metrics := &fakeCacheMetrics{}
	seats := NewCachedSeats(source, cache, metrics)

	got, err := seats.ListByEvent(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("ListByEvent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "seat-1" {
		t.Errorf("seats = %+v", got)
	}
	if got := source.callCount(); got != 1 {
		t.Errorf("source calls = %d, want 1", got)
	}
	if cache.sets != 1 {
		t.Errorf("cache sets = %d, want 1", cache.sets)
	}
	if metrics.misses != 1 {
		t.Errorf("misses = %d, want 1", metrics.misses)
	}
}

func TestCachedSeatsFallsBackOnRedisError(t *testing.T) {
	t.Parallel()

	source := &fakeSeatSource{seats: []reservation.Seat{{ID: "seat-1"}}}
	cache := &fakeSeatCache{getErr: errors.New("redis down"), setErr: errors.New("redis down")}
	metrics := &fakeCacheMetrics{}
	seats := NewCachedSeats(source, cache, metrics)

	got, err := seats.ListByEvent(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("ListByEvent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "seat-1" {
		t.Errorf("seats = %+v", got)
	}
	if got := source.callCount(); got != 1 {
		t.Errorf("source calls = %d, want 1", got)
	}
	if metrics.redisErrors == 0 {
		t.Error("redis errors were not recorded")
	}
}

func TestCachedSeatsSingleflight(t *testing.T) {
	t.Parallel()

	source := &fakeSeatSource{
		seats:   []reservation.Seat{{ID: "seat-1"}},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	cache := &fakeSeatCache{}
	seats := NewCachedSeats(source, cache, nil)

	const callers = 10
	var wg sync.WaitGroup
	results := make(chan []reservation.Seat, callers)
	errs := make(chan error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := seats.ListByEvent(context.Background(), "event-1")
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}

	<-source.started
	time.Sleep(50 * time.Millisecond)
	close(source.release)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("ListByEvent: %v", err)
	}
	for got := range results {
		if len(got) != 1 || got[0].ID != "seat-1" {
			t.Fatalf("seats = %+v", got)
		}
	}
	if got := source.callCount(); got != 1 {
		t.Errorf("source calls = %d, want 1", got)
	}
}
