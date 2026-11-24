package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/zezao/ticket-checkout-system/internal/adapters/httpapi"
	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
)

var (
	databaseURL  string
	testStartsAt = time.Date(2027, 6, 12, 19, 30, 0, 0, time.UTC)
	httpClient   = &http.Client{Timeout: 10 * time.Second}
)

func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("tickets"),
		tcpostgres.WithUsername("tickets"),
		tcpostgres.WithPassword("tickets"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}

	code := func() int {
		defer func() {
			if err := testcontainers.TerminateContainer(container); err != nil {
				log.Printf("terminate postgres container: %v", err)
			}
		}()

		databaseURL, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			log.Printf("read connection string: %v", err)
			return 1
		}
		if err := applyMigrations(); err != nil {
			log.Printf("apply migrations: %v", err)
			return 1
		}
		return m.Run()
	}()

	os.Exit(code)
}

func applyMigrations() error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	migrator, err := migrate.New("file://"+filepath.Join(wd, "..", "..", "migrations"), databaseURL)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() { _, _ = migrator.Close() }()

	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

func TestReadAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)

	t.Run("list events", func(t *testing.T) {
		var body eventsDTO
		resp := getJSON(t, server.URL+"/events", &body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if got := resp.Header.Get("X-Request-ID"); got == "" {
			t.Error("X-Request-ID header is empty")
		}

		event, ok := findEvent(body.Events, eventID)
		if !ok {
			t.Fatalf("event %s missing from response", eventID)
		}
		if event.StartsAt != testStartsAt.Format(time.RFC3339) {
			t.Errorf("starts_at = %q, want %q", event.StartsAt, testStartsAt.Format(time.RFC3339))
		}
		if event.Currency != "USD" {
			t.Errorf("currency = %q, want %q", event.Currency, "USD")
		}
	})

	t.Run("event detail", func(t *testing.T) {
		var body eventDTO
		resp := getJSON(t, server.URL+"/events/"+eventID, &body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if body.ID != eventID {
			t.Errorf("id = %q, want %q", body.ID, eventID)
		}
		if body.Name != "Integration Event" {
			t.Errorf("name = %q, want %q", body.Name, "Integration Event")
		}
	})

	t.Run("event not found", func(t *testing.T) {
		var body errorDTO
		resp := getJSON(t, server.URL+"/events/"+uuid.NewString(), &body)

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
		if body.Error.Code != "not_found" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "not_found")
		}
	})

	t.Run("malformed event id", func(t *testing.T) {
		var body errorDTO
		resp := getJSON(t, server.URL+"/events/not-a-uuid", &body)

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
		if body.Error.Code != "not_found" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "not_found")
		}
	})

	t.Run("seat map", func(t *testing.T) {
		var body seatsDTO
		resp := getJSON(t, server.URL+"/events/"+eventID+"/seats", &body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if len(body.Seats) != len(seatIDs) {
			t.Fatalf("got %d seats, want %d", len(body.Seats), len(seatIDs))
		}
		if body.Seats[0].ID != seatIDs[0] || body.Seats[0].Section != "Balcony" {
			t.Errorf("first seat = %+v", body.Seats[0])
		}
		if body.Seats[1].ID != seatIDs[1] || body.Seats[1].Section != "Orchestra" {
			t.Errorf("second seat = %+v", body.Seats[1])
		}
		for _, seat := range body.Seats {
			if seat.Currency != "USD" {
				t.Errorf("seat currency = %q, want %q", seat.Currency, "USD")
			}
			if seat.Status != "available" {
				t.Errorf("seat status = %q, want %q", seat.Status, "available")
			}
		}
	})

	t.Run("seat map not found", func(t *testing.T) {
		var body errorDTO
		resp := getJSON(t, server.URL+"/events/"+uuid.NewString()+"/seats", &body)

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
		if body.Error.Code != "not_found" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "not_found")
		}
	})

	t.Run("request id passthrough", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/events/"+eventID, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Request-ID", "integration-request")

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if got := resp.Header.Get("X-Request-ID"); got != "integration-request" {
			t.Errorf("X-Request-ID = %q, want %q", got, "integration-request")
		}
	})

	checkInvariants(t, pool)
}

func TestReadiness(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool := newPool(t)
	server := newServer(t, pool)

	var body map[string]string
	resp := getJSON(t, server.URL+"/readyz", &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want %q", body["status"], "ok")
	}
}

func TestReadinessWithoutDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	pool.Close()

	server := newServer(t, pool)

	var body map[string]string
	resp := getJSON(t, server.URL+"/readyz", &body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if body["status"] != "unavailable" {
		t.Errorf("status field = %q, want %q", body["status"], "unavailable")
	}
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	cfg.MaxConns = 50

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	return pool
}

func newServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	return newServerWithPaymentDeadline(t, pool, 15*time.Minute)
}

func newServerWithPaymentDeadline(
	t *testing.T,
	pool *pgxpool.Pool,
	paymentDeadline time.Duration,
) *httptest.Server {
	t.Helper()

	store := pgadapter.NewCatalog(pool)
	holds := pgadapter.NewHoldStore(pool)
	payments := pgadapter.NewPaymentStore(pool)
	handler := httpapi.NewHandler(httpapi.Deps{
		Postgres:     pool,
		Catalog:      app.NewCatalog(store, store),
		Reservations: app.NewReservation(holds, newHoldID, 10*time.Minute),
		Payments:     app.NewPayments(payments, newHoldID, paymentDeadline),
		Logger:       discardLogger(),
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newReservation(pool *pgxpool.Pool) *app.Reservation {
	return app.NewReservation(pgadapter.NewHoldStore(pool), newHoldID, 10*time.Minute)
}

func newHoldID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func checkInvariants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	violations, err := pgadapter.NewInvariantChecker(pool).Check(context.Background())
	if err != nil {
		t.Fatalf("check invariants: %v", err)
	}
	for _, violation := range violations {
		t.Errorf("invariant %s violated: %v", violation.Name, violation.IDs)
	}
}

func insertEvent(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	eventID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO events (id, name, venue, starts_at, currency)
		VALUES ($1::uuid, $2, $3, $4, $5)`,
		eventID, "Integration Event", "Integration Venue", testStartsAt, "USD",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return eventID
}

func insertSeat(
	t *testing.T,
	pool *pgxpool.Pool,
	eventID, section, rowLabel string,
	number int,
	priceCents int64,
) string {
	t.Helper()

	seatID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO seats (id, event_id, section, row_label, number, price_cents)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)`,
		seatID, eventID, section, rowLabel, number, priceCents,
	); err != nil {
		t.Fatalf("insert seat: %v", err)
	}
	return seatID
}

func insertEventWithSeats(t *testing.T, pool *pgxpool.Pool) (string, []string) {
	t.Helper()

	eventID := insertEvent(t, pool)

	seats := []struct {
		section    string
		rowLabel   string
		number     int
		priceCents int64
	}{
		{section: "Balcony", rowLabel: "B", number: 1, priceCents: 4500},
		{section: "Orchestra", rowLabel: "A", number: 1, priceCents: 8500},
	}

	seatIDs := make([]string, 0, len(seats))
	for _, seat := range seats {
		seatIDs = append(seatIDs, insertSeat(
			t, pool, eventID, seat.section, seat.rowLabel, seat.number, seat.priceCents,
		))
	}

	return eventID, seatIDs
}

func getJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()

	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp
}

func findEvent(events []eventDTO, id string) (eventDTO, bool) {
	for _, event := range events {
		if event.ID == id {
			return event, true
		}
	}
	return eventDTO{}, false
}

type eventDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Venue    string `json:"venue"`
	StartsAt string `json:"starts_at"`
	Currency string `json:"currency"`
}

type eventsDTO struct {
	Events []eventDTO `json:"events"`
}

type seatDTO struct {
	ID         string `json:"id"`
	Section    string `json:"section"`
	Row        string `json:"row"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency"`
}

type seatsDTO struct {
	Seats []seatDTO `json:"seats"`
}

type errorDTO struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
