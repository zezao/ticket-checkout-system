package reservation

import (
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)

func TestCanTransitionAllPairs(t *testing.T) {
	t.Parallel()

	statuses := []HoldStatus{
		HoldActive,
		HoldPaymentPending,
		HoldConfirmed,
		HoldExpired,
		HoldReleased,
		HoldPaymentFailed,
	}

	allowed := map[HoldStatus]map[HoldStatus]bool{
		HoldActive: {
			HoldPaymentPending: true,
			HoldReleased:       true,
			HoldExpired:        true,
		},
		HoldPaymentPending: {
			HoldConfirmed:     true,
			HoldPaymentFailed: true,
		},
	}

	for _, from := range statuses {
		for _, to := range statuses {
			err := CanTransition(from, to)
			want := allowed[from][to]

			if want && err != nil {
				t.Errorf("CanTransition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want {
				var transitionErr *TransitionError
				if !errors.As(err, &transitionErr) {
					t.Errorf("CanTransition(%s, %s) = %v, want TransitionError", from, to, err)
				}
			}
		}
	}
}

func TestRelease(t *testing.T) {
	t.Parallel()

	hold := activeHold()

	if _, err := Release(hold, "someone-else"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("wrong owner error = %v, want ErrNotOwner", err)
	}

	next, err := Release(hold, hold.UserID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if next != HoldReleased {
		t.Errorf("status = %s, want %s", next, HoldReleased)
	}

	confirmed := hold
	confirmed.Status = HoldConfirmed
	if _, err := Release(confirmed, hold.UserID); err == nil {
		t.Error("expected a confirmed hold to reject release")
	}
}

func TestExpire(t *testing.T) {
	t.Parallel()

	hold := activeHold()

	if _, err := Expire(hold, hold.ExpiresAt.Add(-time.Second)); !errors.Is(err, ErrHoldNotExpired) {
		t.Errorf("early expiry error = %v, want ErrHoldNotExpired", err)
	}

	next, err := Expire(hold, hold.ExpiresAt)
	if err != nil {
		t.Fatalf("Expire at expiry: %v", err)
	}
	if next != HoldExpired {
		t.Errorf("status = %s, want %s", next, HoldExpired)
	}

	confirmed := hold
	confirmed.Status = HoldConfirmed
	if _, err := Expire(confirmed, hold.ExpiresAt.Add(time.Hour)); err == nil {
		t.Error("expected a confirmed hold to reject expiry")
	}
}

func TestStartPayment(t *testing.T) {
	t.Parallel()

	hold := activeHold()

	if _, err := StartPayment(hold, "someone-else", testNow); !errors.Is(err, ErrNotOwner) {
		t.Errorf("wrong owner error = %v, want ErrNotOwner", err)
	}

	if _, err := StartPayment(hold, hold.UserID, hold.ExpiresAt); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("expired hold error = %v, want ErrHoldExpired", err)
	}

	next, err := StartPayment(hold, hold.UserID, testNow)
	if err != nil {
		t.Fatalf("StartPayment: %v", err)
	}
	if next != HoldPaymentPending {
		t.Errorf("status = %s, want %s", next, HoldPaymentPending)
	}
}

func TestSettlementTransitions(t *testing.T) {
	t.Parallel()

	hold := activeHold()
	if _, err := Confirm(hold); err == nil {
		t.Error("expected an active hold to reject confirmation")
	}

	hold.Status = HoldPaymentPending

	next, err := Confirm(hold)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if next != HoldConfirmed {
		t.Errorf("status = %s, want %s", next, HoldConfirmed)
	}

	next, err = FailPayment(hold)
	if err != nil {
		t.Fatalf("FailPayment: %v", err)
	}
	if next != HoldPaymentFailed {
		t.Errorf("status = %s, want %s", next, HoldPaymentFailed)
	}
}

func FuzzHoldTransitions(f *testing.F) {
	f.Add([]byte{0, 2, 3})
	f.Add([]byte{1, 4})
	f.Add([]byte{2, 4, 0})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, ops []byte) {
		hold := activeHold()

		for _, op := range ops {
			before := hold.Status

			var (
				next HoldStatus
				err  error
			)
			switch op % 5 {
			case 0:
				next, err = Release(hold, hold.UserID)
			case 1:
				next, err = Expire(hold, hold.ExpiresAt.Add(time.Second))
			case 2:
				next, err = StartPayment(hold, hold.UserID, testNow)
			case 3:
				next, err = Confirm(hold)
			case 4:
				next, err = FailPayment(hold)
			}

			if err != nil {
				if hold.Status != before {
					t.Fatalf("status changed from %s to %s after an error", before, hold.Status)
				}
				continue
			}
			if err := CanTransition(before, next); err != nil {
				t.Fatalf("accepted an illegal transition %s -> %s", before, next)
			}
			hold.Status = next
		}

		if !isTerminal(hold.Status) {
			return
		}
		if next, err := Release(hold, hold.UserID); err == nil {
			t.Fatalf("terminal hold %s accepted release to %s", hold.Status, next)
		}
		if next, err := Confirm(hold); err == nil {
			t.Fatalf("terminal hold %s accepted confirmation to %s", hold.Status, next)
		}
		if next, err := FailPayment(hold); err == nil {
			t.Fatalf("terminal hold %s accepted failure to %s", hold.Status, next)
		}
		if next, err := Expire(hold, hold.ExpiresAt.Add(time.Hour)); err == nil {
			t.Fatalf("terminal hold %s accepted expiry to %s", hold.Status, next)
		}
		if next, err := StartPayment(hold, hold.UserID, testNow); err == nil {
			t.Fatalf("terminal hold %s accepted payment start to %s", hold.Status, next)
		}
	})
}

func activeHold() Hold {
	return Hold{
		ID:        "hold-1",
		SeatID:    "seat-1",
		UserID:    "user-1",
		Status:    HoldActive,
		ExpiresAt: testNow.Add(10 * time.Minute),
	}
}

func isTerminal(status HoldStatus) bool {
	switch status {
	case HoldConfirmed, HoldExpired, HoldReleased, HoldPaymentFailed:
		return true
	default:
		return false
	}
}
