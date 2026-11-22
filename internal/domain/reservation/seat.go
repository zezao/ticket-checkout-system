package reservation

// SeatStatus is the lifecycle state of a seat.
type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatSold      SeatStatus = "sold"
)

// Seat is a seat offered for one event.
type Seat struct {
	ID         string
	EventID    string
	Section    string
	RowLabel   string
	Number     int
	PriceCents int64
	Status     SeatStatus
}
