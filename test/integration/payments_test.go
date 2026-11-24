package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zezao/ticket-checkout-system/internal/adapters/mockpay"
	pgadapter "github.com/zezao/ticket-checkout-system/internal/adapters/postgres"
	"github.com/zezao/ticket-checkout-system/internal/app"
	"github.com/zezao/ticket-checkout-system/internal/domain/reservation"
)

func TestPaymentFlowSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)

	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d, want %d", status, http.StatusAccepted)
	}
	if envelope.Payment == nil {
		t.Fatal("payment is nil")
	}
	attemptID := envelope.Payment.ID
	if envelope.Payment.Status != "created" {
		t.Errorf("payment status = %q, want created", envelope.Payment.Status)
	}
	if envelope.Hold.Status != "payment_pending" {
		t.Errorf("hold status = %q, want payment_pending", envelope.Hold.Status)
	}

	assertHoldStatus(t, pool, holdID, "payment_pending")
	assertAttemptStatus(t, pool, attemptID, "created")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}

	status, data := doRequest(t, http.MethodGet, server.URL+"/holds/"+holdID, userID, "")
	if status != http.StatusOK {
		t.Fatalf("get hold status = %d: %s", status, data)
	}
	final := decodeHold(t, data)
	if final.Hold.Status != "confirmed" {
		t.Errorf("hold status = %q, want confirmed", final.Hold.Status)
	}
	if final.Payment == nil || final.Payment.Status != "succeeded" {
		t.Errorf("payment = %+v, want succeeded", final.Payment)
	}

	assertSeatState(t, pool, seatID, seatState{
		Status:        "sold",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})
	assertAttemptStatus(t, pool, attemptID, "succeeded")
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}

	var seatEvents, orderEvents int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		 WHERE topic = 'seat.changed' AND payload->>'seat_id' = $1`, seatID,
	).Scan(&seatEvents); err != nil {
		t.Fatalf("count seat events: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		 WHERE topic = 'order.confirmed' AND payload->>'seat_id' = $1`, seatID,
	).Scan(&orderEvents); err != nil {
		t.Fatalf("count order events: %v", err)
	}
	if seatEvents != 2 {
		t.Errorf("seat.changed rows = %d, want 2", seatEvents)
	}
	if orderEvents != 1 {
		t.Errorf("order.confirmed rows = %d, want 1", orderEvents)
	}

	checkInvariants(t, pool)
}

func TestInvariant04_HoldCannotStartPaymentAfterExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	owner := uuid.NewString()
	other := uuid.NewString()
	holdID := uuid.NewString()
	past := time.Now().Add(-time.Minute).UTC()

	insertHold(t, pool, holdID, seatID, owner, reservation.HoldActive, past)
	holdSeat(t, pool, seatID, holdID, &past)

	status, data, err := rawStartPayment(ctx, server.URL, holdID, owner, "key-1")
	if err != nil {
		t.Fatalf("start payment: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if got := decodeError(t, data).Error.Code; got != "hold_not_active" {
		t.Errorf("error code = %q, want hold_not_active", got)
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}

	status, data = doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", other,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire after expiry status = %d: %s", status, data)
	}
	if got := holdStatus(t, pool, holdID); got != "expired" {
		t.Errorf("old hold status = %q, want expired", got)
	}

	checkInvariants(t, pool)
}

func TestInvariant06_OneChargePerHold(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)

	const calls = 50
	type result struct {
		status    int
		attemptID string
		err       error
	}
	results := make(chan result, calls)
	start := make(chan struct{})

	for i := 0; i < calls; i++ {
		go func() {
			<-start
			status, data, err := rawStartPayment(ctx, server.URL, holdID, userID, "shared-key")
			out := result{status: status, err: err}
			if err == nil && (status == http.StatusOK || status == http.StatusAccepted) {
				var body holdEnvelopeBody
				if decodeErr := json.Unmarshal(data, &body); decodeErr != nil {
					out.err = decodeErr
				} else if body.Payment != nil {
					out.attemptID = body.Payment.ID
				}
			}
			results <- out
		}()
	}
	close(start)

	attemptIDs := make(map[string]struct{})
	for i := 0; i < calls; i++ {
		out := <-results
		if out.err != nil {
			t.Fatalf("concurrent start payment: %v", out.err)
		}
		if out.status != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", out.status, http.StatusAccepted)
		}
		if out.attemptID == "" {
			t.Fatal("empty attempt id")
		}
		attemptIDs[out.attemptID] = struct{}{}
	}
	if len(attemptIDs) != 1 {
		t.Fatalf("distinct attempt ids = %d, want 1", len(attemptIDs))
	}
	var attemptID string
	for id := range attemptIDs {
		attemptID = id
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	status, data, err := rawStartPayment(ctx, server.URL, holdID, userID, "different-key")
	if err != nil {
		t.Fatalf("different key: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("different key status = %d, want %d: %s", status, http.StatusConflict, data)
	}
	if got := decodeError(t, data).Error.Code; got != "payment_in_progress" {
		t.Errorf("error code = %q, want payment_in_progress", got)
	}

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}

	checkInvariants(t, pool)
}

func TestInvariant07_SeatSoldAtMostOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Fatalf("orders = %d, want 1", orders)
	}

	store := pgadapter.NewPaymentStore(pool)
	result, err := store.ApplyResult(ctx, app.SettleParams{
		AttemptID: attemptID,
		Outcome:   app.SettlementSucceeded,
	})
	if err != nil {
		t.Fatalf("duplicate settlement: %v", err)
	}
	if result.Applied {
		t.Error("duplicate settlement was applied")
	}

	if settled := settleOnce(t, ctx, pool, provider); settled != 0 {
		t.Errorf("worker claimed %d final attempts, want 0", settled)
	}
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}
	if charges, err := provider.ChargeCount(ctx, attemptID); err != nil {
		t.Fatalf("charge count: %v", err)
	} else if charges != 1 {
		t.Errorf("charges = %d, want 1", charges)
	}

	checkInvariants(t, pool)
}

func TestFault07_PaymentExpiresWhileWaitingForHoldLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	owner := uuid.NewString()
	other := uuid.NewString()
	holdID := uuid.NewString()
	expiresAt := time.Now().Add(800 * time.Millisecond).UTC()

	insertHold(t, pool, holdID, seatID, owner, reservation.HoldActive, expiresAt)
	holdSeat(t, pool, seatID, holdID, &expiresAt)

	adminConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer adminConn.Release()

	adminTx, err := adminConn.Begin(ctx)
	if err != nil {
		t.Fatalf("admin transaction: %v", err)
	}
	defer func() { _ = adminTx.Rollback(ctx) }()

	var locked string
	if err := adminTx.QueryRow(ctx,
		"SELECT id::text FROM holds WHERE id = $1::uuid FOR UPDATE", holdID,
	).Scan(&locked); err != nil {
		t.Fatalf("lock hold: %v", err)
	}

	type outcome struct {
		status int
		data   []byte
		err    error
	}
	response := make(chan outcome, 1)
	go func() {
		status, data, err := rawStartPayment(ctx, server.URL, holdID, owner, "key-1")
		response <- outcome{status: status, data: data, err: err}
	}()

	waitForBlockedQuery(t, ctx, pool, "%from holds%for update%")

	time.Sleep(time.Until(expiresAt.Add(300 * time.Millisecond)))

	if err := adminTx.Rollback(ctx); err != nil {
		t.Fatalf("release hold lock: %v", err)
	}

	select {
	case got := <-response:
		if got.err != nil {
			t.Fatalf("start payment: %v", got.err)
		}
		if got.status != http.StatusConflict {
			t.Fatalf("status = %d, want %d: %s", got.status, http.StatusConflict, got.data)
		}
		if code := decodeError(t, got.data).Error.Code; code != "hold_not_active" {
			t.Errorf("error code = %q, want hold_not_active", code)
		}
	case <-ctx.Done():
		t.Fatal("payment request did not return")
	}

	if attempts := attemptCount(t, pool, holdID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}

	status, data := doRequest(t, http.MethodPost,
		server.URL+"/events/"+eventID+"/holds", other,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire after expiry status = %d: %s", status, data)
	}

	checkInvariants(t, pool)
}

func TestFault07_GuardBeforeExpirySurvivesLateCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	owner := uuid.NewString()
	holdID := uuid.NewString()
	expiresAt := time.Now().Add(900 * time.Millisecond).UTC()

	insertHold(t, pool, holdID, seatID, owner, reservation.HoldActive, expiresAt)
	holdSeat(t, pool, seatID, holdID, &expiresAt)

	adminConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer adminConn.Release()

	adminTx, err := adminConn.Begin(ctx)
	if err != nil {
		t.Fatalf("admin transaction: %v", err)
	}
	defer func() { _ = adminTx.Rollback(ctx) }()

	var locked string
	if err := adminTx.QueryRow(ctx,
		"SELECT id::text FROM seats WHERE id = $1::uuid FOR UPDATE", seatID,
	).Scan(&locked); err != nil {
		t.Fatalf("lock seat: %v", err)
	}

	type outcome struct {
		status int
		data   []byte
		err    error
	}
	response := make(chan outcome, 1)
	go func() {
		status, data, err := rawStartPayment(ctx, server.URL, holdID, owner, "key-1")
		response <- outcome{status: status, data: data, err: err}
	}()

	waitForBlockedQuery(t, ctx, pool, "%update seats%hold_expires_at%")

	time.Sleep(time.Until(expiresAt.Add(300 * time.Millisecond)))

	if err := adminTx.Rollback(ctx); err != nil {
		t.Fatalf("release seat lock: %v", err)
	}

	select {
	case got := <-response:
		if got.err != nil {
			t.Fatalf("start payment: %v", got.err)
		}
		if got.status != http.StatusAccepted {
			t.Fatalf("status = %d, want %d: %s", got.status, http.StatusAccepted, got.data)
		}
	case <-ctx.Done():
		t.Fatal("payment request did not return")
	}

	assertHoldStatus(t, pool, holdID, "payment_pending")
	assertSeatState(t, pool, seatID, seatState{
		Status:        "held",
		CurrentHoldID: holdID,
		ExpiryNull:    true,
	})

	provider := newMockProvider(t, "success")
	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}

	checkInvariants(t, pool)
}

func TestFault11_StartPaymentDependentUpdateZeroRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID := insertEvent(t, pool)
	seatID := insertSeat(t, pool, eventID, "GA", "A", 1, 8500)

	owner := uuid.NewString()
	holdID := uuid.NewString()
	future := time.Now().Add(10 * time.Minute).UTC()

	insertHold(t, pool, holdID, seatID, owner, reservation.HoldActive, future)
	holdSeat(t, pool, seatID, holdID, &future)

	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
		 WHERE id = $1::uuid`, seatID); err != nil {
		t.Fatalf("corrupt seat: %v", err)
	}

	status, data, err := rawStartPayment(ctx, server.URL, holdID, owner, "key-1")
	if err != nil {
		t.Fatalf("start payment: %v", err)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", status, http.StatusInternalServerError, data)
	}
	if code := decodeError(t, data).Error.Code; code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", code)
	}
	if attempts := attemptCount(t, pool, holdID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
	if got := holdStatus(t, pool, holdID); got != "active" {
		t.Errorf("hold status = %q, want active after rollback", got)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET status = 'held', current_hold_id = $1::uuid,
		       hold_expires_at = (SELECT expires_at FROM holds WHERE id = $1::uuid)
		 WHERE id = $2::uuid`, holdID, seatID); err != nil {
		t.Fatalf("repair seat: %v", err)
	}

	checkInvariants(t, pool)
}

func TestFault11_SettlementDependentUpdateZeroRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx := context.Background()
	pool := newPool(t)
	server := newServer(t, pool)
	eventID, seatIDs := insertEventWithSeats(t, pool)
	seatID := seatIDs[0]
	userID := uuid.NewString()

	holdID := acquireHoldHTTP(t, server.URL, eventID, seatID, userID)
	status, envelope := startPaymentHTTP(t, server.URL, holdID, userID, "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("start payment status = %d", status)
	}
	attemptID := envelope.Payment.ID

	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET status = 'available', current_hold_id = NULL, hold_expires_at = NULL
		 WHERE id = $1::uuid`, seatID); err != nil {
		t.Fatalf("corrupt seat: %v", err)
	}

	store := pgadapter.NewPaymentStore(pool)
	provider := newMockProvider(t, "success")
	settlement := app.NewSettlement(store, provider, 2*time.Second)

	attempts, err := store.ClaimDue(ctx, 200*time.Millisecond, 100)
	if err != nil {
		t.Fatalf("claim due: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("claimed %d attempts, want 1", len(attempts))
	}
	if err := settlement.Settle(ctx, attempts[0]); err == nil {
		t.Fatal("expected a consistency error")
	}

	assertAttemptStatus(t, pool, attemptID, "created")
	assertHoldStatus(t, pool, holdID, "payment_pending")
	if orders := orderCount(t, pool, seatID); orders != 0 {
		t.Errorf("orders = %d, want 0 before recovery", orders)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE seats
		   SET status = 'held', current_hold_id = $1::uuid, hold_expires_at = NULL
		 WHERE id = $2::uuid`, holdID, seatID); err != nil {
		t.Fatalf("repair seat: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE payment_attempts
		   SET next_check_at = clock_timestamp()
		 WHERE id = $1::uuid`, attemptID); err != nil {
		t.Fatalf("make attempt due: %v", err)
	}

	if settled := settleOnce(t, ctx, pool, provider); settled != 1 {
		t.Fatalf("settled %d attempts, want 1", settled)
	}
	if orders := orderCount(t, pool, seatID); orders != 1 {
		t.Errorf("orders = %d, want 1", orders)
	}

	checkInvariants(t, pool)
}

func acquireHoldHTTP(t *testing.T, serverURL, eventID, seatID, userID string) string {
	t.Helper()

	status, data := doRequest(t, http.MethodPost,
		serverURL+"/events/"+eventID+"/holds", userID,
		`{"seat_id":"`+seatID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("acquire hold status = %d, want %d: %s", status, http.StatusCreated, data)
	}
	return decodeHold(t, data).Hold.ID
}

func rawStartPayment(
	ctx context.Context,
	serverURL, holdID, userID, key string,
) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		serverURL+"/holds/"+holdID+"/payment", nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("Idempotency-Key", key)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func startPaymentHTTP(
	t *testing.T,
	serverURL, holdID, userID, key string,
) (int, holdEnvelopeBody) {
	t.Helper()

	status, data, err := rawStartPayment(context.Background(), serverURL, holdID, userID, key)
	if err != nil {
		t.Fatalf("start payment: %v", err)
	}

	var body holdEnvelopeBody
	if status == http.StatusOK || status == http.StatusAccepted {
		body = decodeHold(t, data)
	}
	return status, body
}

func newMockProvider(t *testing.T, behavior string) *mockpay.Provider {
	t.Helper()

	provider, err := mockpay.New(newPool(t), behavior)
	if err != nil {
		t.Fatalf("new mock provider: %v", err)
	}
	return provider
}

func settleOnce(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	provider *mockpay.Provider,
) int {
	t.Helper()

	store := pgadapter.NewPaymentStore(pool)
	settlement := app.NewSettlement(store, provider, 2*time.Second)

	attempts, err := store.ClaimDue(ctx, 200*time.Millisecond, 100)
	if err != nil {
		t.Fatalf("claim due: %v", err)
	}
	for _, attempt := range attempts {
		if err := settlement.Settle(ctx, attempt); err != nil {
			t.Fatalf("settle attempt %s: %v", attempt.ID, err)
		}
	}
	return len(attempts)
}

func waitForBlockedQuery(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	pattern string,
) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := pool.QueryRow(ctx, `
			SELECT pid
			  FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND pid <> pg_backend_pid()
			   AND wait_event_type = 'Lock'
			   AND query ILIKE $1
			 LIMIT 1`, pattern).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("find blocked query: %v", err)
		}

		select {
		case <-ctx.Done():
			t.Fatal("context ended while waiting for a blocked query")
		case <-time.After(20 * time.Millisecond):
		}
	}

	t.Fatalf("no blocked query matching %q", pattern)
	return 0
}

func attemptStatus(t *testing.T, pool *pgxpool.Pool, attemptID string) string {
	t.Helper()

	var status string
	if err := pool.QueryRow(context.Background(),
		"SELECT status::text FROM payment_attempts WHERE id = $1::uuid", attemptID,
	).Scan(&status); err != nil {
		t.Fatalf("read attempt status: %v", err)
	}
	return status
}

func assertAttemptStatus(t *testing.T, pool *pgxpool.Pool, attemptID, want string) {
	t.Helper()

	if got := attemptStatus(t, pool, attemptID); got != want {
		t.Errorf("attempt status = %q, want %q", got, want)
	}
}

func attemptCount(t *testing.T, pool *pgxpool.Pool, holdID string) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM payment_attempts WHERE hold_id = $1::uuid", holdID,
	).Scan(&count); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return count
}

func orderCount(t *testing.T, pool *pgxpool.Pool, seatID string) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM orders WHERE seat_id = $1::uuid", seatID,
	).Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	return count
}

func assertHoldStatus(t *testing.T, pool *pgxpool.Pool, holdID, want string) {
	t.Helper()

	if got := holdStatus(t, pool, holdID); got != want {
		t.Errorf("hold status = %q, want %q", got, want)
	}
}
