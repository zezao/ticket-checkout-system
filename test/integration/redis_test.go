package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zezao/ticket-checkout-system/internal/adapters/httpapi"
	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	redisadapter "github.com/zezao/ticket-checkout-system/internal/adapters/redis"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/worker"
)

func TestSeatMapCacheHitAndRelayInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	client := newRedisClient(t)
	server := newCachedServer(t, pool, client, 10000)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	userID := uuid.NewString()
	key := "seatmap:" + eventID

	status, data := doRequest(t, http.MethodGet, server.URL+"/events/"+eventID+"/seats", "", "")
	if status != http.StatusOK {
		t.Fatalf("seat map status = %d: %s", status, data)
	}
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 1 {
		t.Fatalf("seat map cache key exists = %d, want 1", exists)
	}

	acquireHoldHTTP(t, server.URL, eventID, seatIDs[0], userID)

	status, data = doRequest(t, http.MethodGet, server.URL+"/events/"+eventID+"/seats", "", "")
	if status != http.StatusOK {
		t.Fatalf("cached seat map status = %d: %s", status, data)
	}
	cached := decodeSeats(t, data)
	if got := seatStatusByID(cached, seatIDs[0]); got != "available" {
		t.Errorf("cached seat status = %q, want stale available", got)
	}

	outboxStore := pgadapter.NewOutboxStore(pool)
	handlers := map[string]worker.OutboxHandler{
		"seat.changed":    worker.NewSeatChangedHandler(redisadapter.NewSeatMapCache(client), discardLogger(), nil),
		"order.confirmed": worker.NoopHandler{},
	}
	relay := worker.NewOutboxRelay(outboxStore, handlers, discardLogger(),
		time.Second, 50*time.Millisecond, nil)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(runCtx)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		exists, err := client.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("redis exists: %v", err)
		}

		var published bool
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(
			    (SELECT published_at IS NOT NULL
			       FROM outbox
			      WHERE topic = 'seat.changed'
			        AND payload->>'seat_id' = $1
			      ORDER BY id DESC
			      LIMIT 1),
			    false
			)`, seatIDs[0]).Scan(&published); err != nil {
			t.Fatalf("read outbox row: %v", err)
		}

		if exists == 0 && published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache invalidation did not finish (exists=%d published=%t)", exists, published)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	status, data = doRequest(t, http.MethodGet, server.URL+"/events/"+eventID+"/seats", "", "")
	if status != http.StatusOK {
		t.Fatalf("fresh seat map status = %d: %s", status, data)
	}
	fresh := decodeSeats(t, data)
	if got := seatStatusByID(fresh, seatIDs[0]); got != "held" {
		t.Errorf("fresh seat status = %q, want held", got)
	}

	var published bool
	if err := pool.QueryRow(ctx, `
		SELECT published_at IS NOT NULL
		  FROM outbox
		 WHERE topic = 'seat.changed'
		   AND payload->>'seat_id' = $1
		 ORDER BY id DESC
		 LIMIT 1`, seatIDs[0]).Scan(&published); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if !published {
		t.Error("seat.changed outbox row was not marked published")
	}

	checkInvariants(t, pool)
}

func TestSeatMapServedFromCacheUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	client := newRedisClient(t)
	server := newCachedServer(t, pool, client, 10000)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	key := "seatmap:" + eventID

	status, data := doRequest(t, http.MethodGet, server.URL+"/events/"+eventID+"/seats", "", "")
	if status != http.StatusOK {
		t.Fatalf("seat map status = %d: %s", status, data)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE seats SET price_cents = 9999 WHERE id = $1::uuid", seatIDs[0],
	); err != nil {
		t.Fatalf("change seat price: %v", err)
	}

	const callers = 50
	type result struct {
		status int
		price  int64
		err    error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			status, data, err := rawRequest(ctx, http.MethodGet,
				server.URL+"/events/"+eventID+"/seats", "", "")
			out := result{status: status, err: err}
			if err == nil && status == http.StatusOK {
				var seats seatsDTO
				if decodeErr := json.Unmarshal(data, &seats); decodeErr != nil {
					out.err = decodeErr
				} else {
					for _, seat := range seats.Seats {
						if seat.ID == seatIDs[0] {
							out.price = seat.PriceCents
						}
					}
				}
			}
			results <- out
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	for out := range results {
		if out.err != nil {
			t.Fatalf("seat map request: %v", out.err)
		}
		if out.status != http.StatusOK {
			t.Fatalf("seat map status = %d, want 200", out.status)
		}
		if out.price != 4500 {
			t.Fatalf("seat price = %d, want cached 4500", out.price)
		}
	}

	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 1 {
		t.Errorf("seat map cache key exists = %d, want 1", exists)
	}

	checkInvariants(t, pool)
}

func TestRateLimitHoldRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	pool := newPool(t)
	client := newRedisClient(t)
	server := newCachedServer(t, pool, client, 2)
	eventID := insertEvent(t, pool)

	seatIDs := []string{
		insertSeat(t, pool, eventID, "GA", "A", 1, 8500),
		insertSeat(t, pool, eventID, "GA", "A", 2, 8500),
		insertSeat(t, pool, eventID, "GA", "A", 3, 8500),
	}

	userOne := uuid.NewString()
	userTwo := uuid.NewString()

	for i := 0; i < 2; i++ {
		status, data := doRequest(t, http.MethodPost,
			server.URL+"/events/"+eventID+"/holds", userOne,
			`{"seat_id":"`+seatIDs[i]+`"}`)
		if status != http.StatusCreated {
			t.Fatalf("hold %d status = %d, want 201: %s", i, status, data)
		}
	}

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userOne,
		`{"seat_id":"`+seatIDs[2]+`"}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("rate limited status = %d, want 429: %s", status, data)
	}
	if code := decodeError(t, data).Error.Code; code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", userTwo,
		`{"seat_id":"`+seatIDs[2]+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("other user status = %d, want 201: %s", status, data)
	}

	checkInvariants(t, pool)
}

func TestInvariant10_RedisStoppedFailsOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	options, err := goredis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	options.DialTimeout = 200 * time.Millisecond
	options.ReadTimeout = 200 * time.Millisecond
	options.WriteTimeout = 200 * time.Millisecond
	options.MaxRetries = 0
	client := goredis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping shared redis: %v", err)
	}

	pool := newPool(t)
	server := newCachedServer(t, pool, client, 10000)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	userID := uuid.NewString()

	stopTimeout := 10 * time.Second
	if err := redisContainer.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop redis: %v", err)
	}
	t.Cleanup(func() {
		if err := redisContainer.Start(context.Background()); err != nil {
			t.Logf("restart shared redis: %v", err)
		}
	})

	status, data := doRequest(t, http.MethodGet, server.URL+"/events/"+eventID+"/seats", "", "")
	if status != http.StatusOK {
		t.Fatalf("seat map with redis down status = %d: %s", status, data)
	}

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatIDs[0], userID)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "redis-down-key")
	if status != http.StatusAccepted {
		t.Fatalf("payment with redis down status = %d", status)
	}
	attemptID := envelope.Payment.ID

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled < 1 {
		t.Fatalf("settled %d attempts, want at least 1", settled)
	}
	assertAttemptStatus(t, pool, attemptID, "succeeded")
	assertHoldStatus(t, pool, holdID, "confirmed")

	status, data = doRequest(t, http.MethodGet, server.URL+"/readyz", "", "")
	if status != http.StatusOK {
		t.Fatalf("readyz with redis down status = %d: %s", status, data)
	}
	var ready struct {
		Status string `json:"status"`
		Redis  string `json:"redis"`
	}
	if err := json.Unmarshal(data, &ready); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if ready.Status != "ok" {
		t.Errorf("ready status = %q, want ok", ready.Status)
	}
	if ready.Redis != "unavailable" {
		t.Errorf("redis field = %q, want unavailable", ready.Redis)
	}

	checkInvariants(t, pool)
}

func newRedisClient(t *testing.T) *goredis.Client {
	t.Helper()

	client, err := redisadapter.NewClient(redisURL)
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return client
}

func newCachedServer(
	t *testing.T,
	pool *pgxpool.Pool,
	client *goredis.Client,
	limit int,
) *httptest.Server {
	t.Helper()

	pgCatalog := pgadapter.NewCatalog(pool)
	seatCache := redisadapter.NewSeatMapCache(client)
	seatStore := redisadapter.NewCachedSeats(pgCatalog, seatCache, nil)
	holds := pgadapter.NewHoldStore(pool)
	payments := pgadapter.NewPaymentStore(pool)

	handler := httpapi.NewHandler(httpapi.Deps{
		Postgres:     pool,
		Redis:        redisadapter.NewPinger(client),
		Catalog:      app.NewCatalog(pgCatalog, seatStore),
		Reservations: app.NewReservation(holds, newHoldID, 10*time.Minute),
		Payments:     app.NewPayments(payments, newHoldID, 15*time.Minute),
		RateLimiter:  redisadapter.NewLimiter(client, limit, nil),
		Logger:       discardLogger(),
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func decodeSeats(t *testing.T, data []byte) seatsDTO {
	t.Helper()

	var body seatsDTO
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode seats: %v", err)
	}
	return body
}

func seatStatusByID(seats seatsDTO, id string) string {
	for _, seat := range seats.Seats {
		if seat.ID == id {
			return seat.Status
		}
	}
	return ""
}
