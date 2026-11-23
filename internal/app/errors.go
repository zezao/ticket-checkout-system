package app

import "errors"

// ErrNotFound reports that a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrSeatUnavailable reports that another live hold already owns the seat.
var ErrSeatUnavailable = errors.New("seat unavailable")

// ErrHoldNotActive reports that the hold is not in a state that allows the change.
var ErrHoldNotActive = errors.New("hold not active")
