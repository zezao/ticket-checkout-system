package payment

import (
	"errors"
	"testing"
)

func TestPaymentCanTransitionAllPairs(t *testing.T) {
	t.Parallel()

	statuses := []Status{StatusCreated, StatusPending, StatusSucceeded, StatusFailed}
	allowed := map[Status]map[Status]bool{
		StatusCreated: {
			StatusPending:   true,
			StatusSucceeded: true,
			StatusFailed:    true,
		},
		StatusPending: {
			StatusPending:   true,
			StatusSucceeded: true,
			StatusFailed:    true,
		},
	}

	for _, from := range statuses {
		for _, to := range statuses {
			err := CanTransition(from, to)
			want := allowed[from][to]
			if want && err != nil {
				t.Errorf("CanTransition(%s, %s) = %v, want nil", from, to, err)
			}
			if !want && err == nil {
				t.Errorf("CanTransition(%s, %s) = nil, want error", from, to)
			}
		}
	}
}

func TestPaymentNextStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		current Status
		outcome Outcome
		want    Status
	}{
		{current: StatusCreated, outcome: OutcomeSucceeded, want: StatusSucceeded},
		{current: StatusCreated, outcome: OutcomeDeclined, want: StatusFailed},
		{current: StatusCreated, outcome: OutcomeProcessing, want: StatusPending},
		{current: StatusCreated, outcome: OutcomeUnknown, want: StatusPending},
		{current: StatusPending, outcome: OutcomeSucceeded, want: StatusSucceeded},
		{current: StatusPending, outcome: OutcomeDeclined, want: StatusFailed},
		{current: StatusPending, outcome: OutcomeProcessing, want: StatusPending},
		{current: StatusPending, outcome: OutcomeUnknown, want: StatusPending},
	}

	for _, tt := range tests {
		got, err := NextStatus(tt.current, tt.outcome)
		if err != nil {
			t.Errorf("NextStatus(%s, %s): %v", tt.current, tt.outcome, err)
			continue
		}
		if got != tt.want {
			t.Errorf("NextStatus(%s, %s) = %s, want %s", tt.current, tt.outcome, got, tt.want)
		}
	}
}

func TestPaymentNextStatusRejectsFinal(t *testing.T) {
	t.Parallel()

	for _, status := range []Status{StatusSucceeded, StatusFailed} {
		if _, err := NextStatus(status, OutcomeSucceeded); !errors.Is(err, ErrFinal) {
			t.Errorf("NextStatus(%s) error = %v, want ErrFinal", status, err)
		}
	}
}

func TestAttemptOpen(t *testing.T) {
	t.Parallel()

	if !(Attempt{Status: StatusCreated}).Open() {
		t.Error("created attempt should be open")
	}
	if !(Attempt{Status: StatusPending}).Open() {
		t.Error("pending attempt should be open")
	}
	for _, status := range []Status{StatusSucceeded, StatusFailed} {
		if (Attempt{Status: status}).Open() {
			t.Errorf("%s attempt should be final", status)
		}
	}
}
