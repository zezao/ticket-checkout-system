package payment

import (
	"errors"
	"fmt"
	"time"
)

// Status is the lifecycle state of a payment attempt.
type Status string

const (
	StatusCreated   Status = "created"
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// ErrFinal reports that no further provider result may change the attempt.
var ErrFinal = errors.New("payment attempt is final")

// Outcome is the provider's answer for one attempt.
type Outcome string

const (
	OutcomeSucceeded  Outcome = "succeeded"
	OutcomeDeclined   Outcome = "declined"
	OutcomeProcessing Outcome = "processing"
	OutcomeUnknown    Outcome = "unknown"
)

// Attempt is one payment attempt for one hold.
type Attempt struct {
	ID             string
	HoldID         string
	UserID         string
	IdempotencyKey string
	AmountCents    int64
	Currency       string
	Status         Status
	Attempts       int
	NextCheckAt    time.Time
	DeadlineAt     time.Time
	LastError      string
	Traceparent    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Open reports whether the attempt can still accept a provider result.
func (a Attempt) Open() bool {
	return a.Status == StatusCreated || a.Status == StatusPending
}

// CanTransition reports whether the attempt lifecycle allows from -> to.
func CanTransition(from, to Status) error {
	switch from {
	case StatusCreated, StatusPending:
		switch to {
		case StatusPending, StatusSucceeded, StatusFailed:
			return nil
		}
	}
	return fmt.Errorf("illegal payment transition from %s to %s", from, to)
}

// NextStatus maps a provider outcome onto the attempt lifecycle.
func NextStatus(current Status, outcome Outcome) (Status, error) {
	if !(Attempt{Status: current}).Open() {
		return current, ErrFinal
	}

	var next Status
	switch outcome {
	case OutcomeSucceeded:
		next = StatusSucceeded
	case OutcomeDeclined:
		next = StatusFailed
	case OutcomeProcessing, OutcomeUnknown:
		next = StatusPending
	default:
		return current, fmt.Errorf("unknown payment outcome %q", outcome)
	}

	if err := CanTransition(current, next); err != nil {
		return current, err
	}
	return next, nil
}
