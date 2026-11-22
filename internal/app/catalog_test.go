package app

import (
	"context"
	"errors"
	"testing"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type stubEventStore struct {
	events []Event
	event  Event
	err    error
}

func (s stubEventStore) ListEvents(context.Context) ([]Event, error) {
	return s.events, s.err
}

func (s stubEventStore) GetEvent(context.Context, string) (Event, error) {
	return s.event, s.err
}

type stubSeatStore struct {
	seats []reservation.Seat
	err   error
	calls int
}

func (s *stubSeatStore) ListByEvent(context.Context, string) ([]reservation.Seat, error) {
	s.calls++
	return s.seats, s.err
}

func TestCatalogListEvents(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: "one"}, {ID: "two"}}
	catalog := NewCatalog(stubEventStore{events: events}, &stubSeatStore{})

	got, err := catalog.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d", len(got), len(events))
	}
}

func TestCatalogGetEvent(t *testing.T) {
	t.Parallel()

	catalog := NewCatalog(stubEventStore{event: Event{ID: "event-1"}}, &stubSeatStore{})

	got, err := catalog.GetEvent(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.ID != "event-1" {
		t.Errorf("ID = %q, want %q", got.ID, "event-1")
	}
}

func TestCatalogSeatMap(t *testing.T) {
	t.Parallel()

	seats := &stubSeatStore{seats: []reservation.Seat{{ID: "seat-1"}}}
	catalog := NewCatalog(stubEventStore{event: Event{ID: "event-1", Currency: "USD"}}, seats)

	got, err := catalog.SeatMap(context.Background(), "event-1")
	if err != nil {
		t.Fatalf("SeatMap: %v", err)
	}
	if got.Event.ID != "event-1" {
		t.Errorf("Event.ID = %q, want %q", got.Event.ID, "event-1")
	}
	if len(got.Seats) != 1 {
		t.Errorf("got %d seats, want 1", len(got.Seats))
	}
	if seats.calls != 1 {
		t.Errorf("seat store calls = %d, want 1", seats.calls)
	}
}

func TestCatalogSeatMapSkipsSeatsWhenEventMissing(t *testing.T) {
	t.Parallel()

	seats := &stubSeatStore{}
	catalog := NewCatalog(stubEventStore{err: ErrNotFound}, seats)

	if _, err := catalog.SeatMap(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SeatMap error = %v, want ErrNotFound", err)
	}
	if seats.calls != 0 {
		t.Errorf("seat store calls = %d, want 0", seats.calls)
	}
}
