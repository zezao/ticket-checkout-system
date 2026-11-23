package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

type stubHoldStore struct {
	params         AcquireParams
	result         AcquireResult
	acquireErr     error
	releasedHoldID string
	releasedUserID string
	releaseErr     error
	gotHoldID      string
	gotUserID      string
	hold           reservation.Hold
	getErr         error
}

func (s *stubHoldStore) Acquire(_ context.Context, params AcquireParams) (AcquireResult, error) {
	s.params = params
	return s.result, s.acquireErr
}

func (s *stubHoldStore) Release(_ context.Context, holdID, userID string) error {
	s.releasedHoldID = holdID
	s.releasedUserID = userID
	return s.releaseErr
}

func (s *stubHoldStore) Get(_ context.Context, holdID, userID string) (reservation.Hold, error) {
	s.gotHoldID = holdID
	s.gotUserID = userID
	return s.hold, s.getErr
}

func TestReservationAcquireBuildsParams(t *testing.T) {
	t.Parallel()

	store := &stubHoldStore{result: AcquireResult{Hold: reservation.Hold{ID: "hold-1"}, Created: true}}
	reservationUseCase := NewReservation(store, func() (string, error) {
		return "generated-hold", nil
	}, 5*time.Minute)

	result, err := reservationUseCase.Acquire(context.Background(), AcquireRequest{
		EventID: "event-1",
		SeatID:  "seat-1",
		UserID:  "user-1",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !result.Created {
		t.Error("Created = false, want true")
	}

	want := AcquireParams{
		HoldID:  "generated-hold",
		EventID: "event-1",
		SeatID:  "seat-1",
		UserID:  "user-1",
		TTL:     5 * time.Minute,
	}
	if store.params != want {
		t.Errorf("params = %+v, want %+v", store.params, want)
	}
}

func TestReservationAcquireGeneratorError(t *testing.T) {
	t.Parallel()

	store := &stubHoldStore{}
	reservationUseCase := NewReservation(store, func() (string, error) {
		return "", errors.New("no entropy")
	}, time.Minute)

	if _, err := reservationUseCase.Acquire(context.Background(), AcquireRequest{}); err == nil {
		t.Fatal("expected an error")
	}
	if store.params != (AcquireParams{}) {
		t.Error("store was called after the id generator failed")
	}
}

func TestReservationRelease(t *testing.T) {
	t.Parallel()

	store := &stubHoldStore{}
	reservationUseCase := NewReservation(store, func() (string, error) { return "", nil }, time.Minute)

	if err := reservationUseCase.Release(context.Background(), "hold-1", "user-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if store.releasedHoldID != "hold-1" || store.releasedUserID != "user-1" {
		t.Errorf("release called with %q %q", store.releasedHoldID, store.releasedUserID)
	}
}

func TestReservationGet(t *testing.T) {
	t.Parallel()

	store := &stubHoldStore{hold: reservation.Hold{ID: "hold-1"}}
	reservationUseCase := NewReservation(store, func() (string, error) { return "", nil }, time.Minute)

	hold, err := reservationUseCase.Get(context.Background(), "hold-1", "user-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hold.ID != "hold-1" {
		t.Errorf("ID = %q", hold.ID)
	}
	if store.gotHoldID != "hold-1" || store.gotUserID != "user-1" {
		t.Errorf("get called with %q %q", store.gotHoldID, store.gotUserID)
	}
}
