package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/adapters/httpapi"
	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/observability"
)

func TestMetricsEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool := newPool(t)
	metrics := observability.NewMetrics()
	server := newMetricsServer(t, pool, metrics)
	eventID, seatIDs := insertEventWithSeats(t, pool)

	owner := uuid.NewString()
	other := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatIDs[0], owner)

	status, _ := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", other,
		`{"seat_id":"`+seatIDs[0]+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", status)
	}

	status, _ = startPaymentHTTP(t, server.URL, holdID, owner, "metrics-key")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d, want 202", status)
	}

	status, data := doRequest(t, http.MethodGet, server.URL+"/metrics", "", "")
	if status != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", status, data)
	}
	body := string(data)

	wants := []string{
		`hold_attempts_total{result="acquired"} 1`,
		`hold_attempts_total{result="conflict"} 1`,
		`hold_acquire_duration_seconds_count 2`,
		`payments_total{result="started"} 1`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output does not contain %q", want)
		}
	}

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, context.Background(), pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	checkInvariants(t, pool)
}

func newMetricsServer(
	t *testing.T,
	pool *pgxpool.Pool,
	metrics *observability.Metrics,
) *httptest.Server {
	t.Helper()

	store := pgadapter.NewCatalog(pool)
	holds := pgadapter.NewHoldStore(pool)
	payments := pgadapter.NewPaymentStore(pool)
	handler := httpapi.NewHandler(httpapi.Deps{
		Postgres:       pool,
		Catalog:        app.NewCatalog(store, store),
		Reservations:   app.NewReservation(holds, newHoldID, 10*time.Minute),
		Payments:       app.NewPayments(payments, newHoldID, 15*time.Minute),
		Metrics:        metrics,
		MetricsHandler: metrics.Handler(),
		Logger:         discardLogger(),
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}
