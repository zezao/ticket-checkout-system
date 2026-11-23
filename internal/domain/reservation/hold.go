package reservation

import (
	"errors"
	"fmt"
	"time"
)

// HoldStatus is the lifecycle state of a hold.
type HoldStatus string

const (
	HoldActive         HoldStatus = "active"
	HoldPaymentPending HoldStatus = "payment_pending"
	HoldConfirmed      HoldStatus = "confirmed"
	HoldExpired        HoldStatus = "expired"
	HoldReleased       HoldStatus = "released"
	HoldPaymentFailed  HoldStatus = "payment_failed"
)

var (
	// ErrNotOwner reports that the caller does not own the hold.
	ErrNotOwner = errors.New("hold belongs to another user")
	// ErrHoldExpired reports that the hold can no longer start a payment.
	ErrHoldExpired = errors.New("hold has expired")
	// ErrHoldNotExpired reports that the hold is not old enough to expire.
	ErrHoldNotExpired = errors.New("hold has not expired")
)

// TransitionError reports a state change the hold lifecycle does not allow.
type TransitionError struct {
	From HoldStatus
	To   HoldStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal hold transition from %s to %s", e.From, e.To)
}

// Hold is a time-limited claim on one seat by one user.
type Hold struct {
	ID          string
	SeatID      string
	UserID      string
	Status      HoldStatus
	ExpiresAt   time.Time
	AmountCents int64
	Currency    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CanTransition reports whether the hold lifecycle allows from -> to.
func CanTransition(from, to HoldStatus) error {
	switch from {
	case HoldActive:
		switch to {
		case HoldPaymentPending, HoldReleased, HoldExpired:
			return nil
		}
	case HoldPaymentPending:
		switch to {
		case HoldConfirmed, HoldPaymentFailed:
			return nil
		}
	}
	return &TransitionError{From: from, To: to}
}

// Release returns the released status when the owner may release the hold.
func Release(hold Hold, userID string) (HoldStatus, error) {
	if hold.UserID != userID {
		return hold.Status, ErrNotOwner
	}
	if err := CanTransition(hold.Status, HoldReleased); err != nil {
		return hold.Status, err
	}
	return HoldReleased, nil
}

// Expire returns the expired status when the hold has reached its expiry.
func Expire(hold Hold, now time.Time) (HoldStatus, error) {
	if err := CanTransition(hold.Status, HoldExpired); err != nil {
		return hold.Status, err
	}
	if now.Before(hold.ExpiresAt) {
		return hold.Status, ErrHoldNotExpired
	}
	return HoldExpired, nil
}

// StartPayment returns the payment_pending status when the owner may pay.
func StartPayment(hold Hold, userID string, now time.Time) (HoldStatus, error) {
	if hold.UserID != userID {
		return hold.Status, ErrNotOwner
	}
	if err := CanTransition(hold.Status, HoldPaymentPending); err != nil {
		return hold.Status, err
	}
	if !now.Before(hold.ExpiresAt) {
		return hold.Status, ErrHoldExpired
	}
	return HoldPaymentPending, nil
}

// Confirm returns the confirmed status when payment settlement succeeds.
func Confirm(hold Hold) (HoldStatus, error) {
	if err := CanTransition(hold.Status, HoldConfirmed); err != nil {
		return hold.Status, err
	}
	return HoldConfirmed, nil
}

// FailPayment returns the payment_failed status when the provider declines.
func FailPayment(hold Hold) (HoldStatus, error) {
	if err := CanTransition(hold.Status, HoldPaymentFailed); err != nil {
		return hold.Status, err
	}
	return HoldPaymentFailed, nil
}
