package app

import "errors"

// ErrNotFound reports that a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrSeatUnavailable reports that another live hold already owns the seat.
var ErrSeatUnavailable = errors.New("seat unavailable")

// ErrHoldNotActive reports that the hold is not in a state that allows the change.
var ErrHoldNotActive = errors.New("hold not active")

// ErrPaymentInProgress reports that the hold already has an open attempt with another key.
var ErrPaymentInProgress = errors.New("payment in progress")

// ErrIdempotencyKeyReused reports that the key was already used on another hold.
var ErrIdempotencyKeyReused = errors.New("idempotency key reused")
