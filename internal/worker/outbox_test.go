package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type stubOutboxStore struct {
	messages  []app.OutboxMessage
	claimErr  error
	published [][]int64
	markErr   error
	lag       time.Duration
	lagErr    error
}

func (s *stubOutboxStore) ClaimOutbox(context.Context, time.Duration, int) ([]app.OutboxMessage, error) {
	return s.messages, s.claimErr
}

func (s *stubOutboxStore) MarkPublished(_ context.Context, ids []int64) error {
	s.published = append(s.published, ids)
	return s.markErr
}

func (s *stubOutboxStore) OutboxLag(context.Context) (time.Duration, error) {
	return s.lag, s.lagErr
}

type stubOutboxHandler struct {
	handled []int64
	fail    map[int64]bool
}

func (h *stubOutboxHandler) Handle(_ context.Context, message app.OutboxMessage) error {
	h.handled = append(h.handled, message.ID)
	if h.fail[message.ID] {
		return errors.New("handler failed")
	}
	return nil
}

type stubOutboxMetrics struct {
	lags             []time.Duration
	consistencyCalls int
}

func (m *stubOutboxMetrics) OutboxLag(lag time.Duration) {
	m.lags = append(m.lags, lag)
}

func (m *stubOutboxMetrics) ConsistencyError() {
	m.consistencyCalls++
}

func TestOutboxRelayPublishesSuccessfulHandlers(t *testing.T) {
	t.Parallel()

	handler := &stubOutboxHandler{fail: map[int64]bool{3: true}}
	store := &stubOutboxStore{
		messages: []app.OutboxMessage{
			{ID: 1, Topic: "seat.changed"},
			{ID: 2, Topic: "unknown.topic"},
			{ID: 3, Topic: "seat.changed"},
		},
		lag: 5 * time.Second,
	}
	metrics := &stubOutboxMetrics{}

	relay := NewOutboxRelay(store, map[string]OutboxHandler{"seat.changed": handler},
		workerTestLogger(), time.Second, time.Minute, metrics)
	relay.tick(context.Background())

	if len(handler.handled) != 2 || handler.handled[0] != 1 || handler.handled[1] != 3 {
		t.Errorf("handled ids = %v, want [1 3]", handler.handled)
	}
	if len(store.published) != 1 {
		t.Fatalf("mark published calls = %d, want 1", len(store.published))
	}
	if got := store.published[0]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("published ids = %v, want [1 2]", got)
	}
	if len(metrics.lags) != 1 || metrics.lags[0] != 5*time.Second {
		t.Errorf("lag metrics = %v, want [5s]", metrics.lags)
	}
}

func TestOutboxRelayClaimError(t *testing.T) {
	t.Parallel()

	store := &stubOutboxStore{claimErr: errors.New("database unavailable")}
	handler := &stubOutboxHandler{}
	metrics := &stubOutboxMetrics{}

	relay := NewOutboxRelay(store, map[string]OutboxHandler{"seat.changed": handler},
		workerTestLogger(), time.Second, time.Minute, metrics)
	relay.tick(context.Background())

	if len(handler.handled) != 0 {
		t.Errorf("handled ids = %v, want none", handler.handled)
	}
	if len(store.published) != 0 {
		t.Errorf("mark published calls = %d, want 0", len(store.published))
	}
	if len(metrics.lags) != 0 {
		t.Errorf("lag metrics = %v, want none", metrics.lags)
	}
}

func TestOutboxRelayEmptyBatchStillRecordsLag(t *testing.T) {
	t.Parallel()

	store := &stubOutboxStore{lag: 2 * time.Second}
	metrics := &stubOutboxMetrics{}

	relay := NewOutboxRelay(store, nil, workerTestLogger(), time.Second, time.Minute, metrics)
	relay.tick(context.Background())

	if len(store.published) != 0 {
		t.Errorf("mark published calls = %d, want 0", len(store.published))
	}
	if len(metrics.lags) != 1 {
		t.Errorf("lag metrics = %v, want one entry", metrics.lags)
	}
}

func TestOutboxRelayRecordsConsistencyErrors(t *testing.T) {
	t.Parallel()

	handler := &stubOutboxHandler{}
	store := &stubOutboxStore{
		messages: []app.OutboxMessage{{ID: 1, Topic: "seat.changed"}},
		markErr:  app.ErrConsistency,
	}
	metrics := &stubOutboxMetrics{}

	relay := NewOutboxRelay(store, map[string]OutboxHandler{"seat.changed": handler},
		workerTestLogger(), time.Second, time.Minute, metrics)
	relay.tick(context.Background())

	if metrics.consistencyCalls != 1 {
		t.Errorf("consistency errors = %d, want 1", metrics.consistencyCalls)
	}
}

func TestNoopHandler(t *testing.T) {
	t.Parallel()

	if err := (NoopHandler{}).Handle(context.Background(), app.OutboxMessage{}); err != nil {
		t.Errorf("NoopHandler: %v", err)
	}
}

func TestSeatChangedHandler(t *testing.T) {
	t.Parallel()

	cache := &stubSeatMapCache{}
	handler := NewSeatChangedHandler(cache, nil, nil)

	message := app.OutboxMessage{
		Topic:   "seat.changed",
		Payload: []byte(`{"event_id":"event-1","seat_id":"seat-1"}`),
	}
	if err := handler.Handle(context.Background(), message); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(cache.deleted) != 1 || cache.deleted[0] != "event-1" {
		t.Errorf("deleted = %v, want [event-1]", cache.deleted)
	}
}

func TestSeatChangedHandlerRejectsBadPayload(t *testing.T) {
	t.Parallel()

	handler := NewSeatChangedHandler(&stubSeatMapCache{}, nil, nil)

	for _, payload := range []string{`{`, `{}`, `{"seat_id":"seat-1"}`} {
		if err := handler.Handle(context.Background(), app.OutboxMessage{Payload: []byte(payload)}); err == nil {
			t.Errorf("payload %q: expected an error", payload)
		}
	}
}

type stubSeatMapCache struct {
	deleted []string
	err     error
}

func (c *stubSeatMapCache) Get(context.Context, string) ([]reservation.Seat, bool, error) {
	return nil, false, nil
}

func (c *stubSeatMapCache) Set(context.Context, string, []reservation.Seat) error {
	return c.err
}

func (c *stubSeatMapCache) Delete(_ context.Context, eventID string) error {
	c.deleted = append(c.deleted, eventID)
	return c.err
}

type stubSeatMapMetrics struct {
	redisErrors int
}

func (m *stubSeatMapMetrics) RedisError() {
	m.redisErrors++
}

func TestSeatChangedHandlerRecordsRedisErrors(t *testing.T) {
	t.Parallel()

	cache := &stubSeatMapCache{err: errors.New("redis down")}
	metrics := &stubSeatMapMetrics{}
	handler := NewSeatChangedHandler(cache, nil, metrics)

	message := app.OutboxMessage{
		Payload: []byte(`{"event_id":"event-1","seat_id":"seat-1"}`),
	}
	if err := handler.Handle(context.Background(), message); err == nil {
		t.Fatal("expected an error")
	}
	if metrics.redisErrors != 1 {
		t.Errorf("redis errors = %d, want 1", metrics.redisErrors)
	}
}
