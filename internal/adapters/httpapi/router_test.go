package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubPinger struct {
	err error
}

func (s stubPinger) Ping(context.Context) error {
	return s.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	NewHandler(Deps{Postgres: stubPinger{}, Logger: testLogger()}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want %q", body["status"], "ok")
	}
}

func TestReadyzReady(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	NewHandler(Deps{Postgres: stubPinger{}, Logger: testLogger()}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want %q", body["status"], "ok")
	}
}

func TestReadyzUnavailable(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	NewHandler(Deps{Postgres: stubPinger{err: errors.New("database down")}, Logger: testLogger()}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "unavailable" {
		t.Errorf("status field = %q, want %q", body.Status, "unavailable")
	}
	if body.Postgres != "unavailable" {
		t.Errorf("postgres field = %q, want %q", body.Postgres, "unavailable")
	}
	if body.Redis != "disabled" {
		t.Errorf("redis field = %q, want %q", body.Redis, "disabled")
	}
}

func TestReadyzReportsRedis(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		redis     Pinger
		wantRedis string
	}{
		{name: "disabled", redis: nil, wantRedis: "disabled"},
		{name: "ok", redis: stubPinger{}, wantRedis: "ok"},
		{name: "unavailable", redis: stubPinger{err: errors.New("redis down")}, wantRedis: "unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

			NewHandler(Deps{
				Postgres: stubPinger{},
				Redis:    tt.redis,
				Logger:   testLogger(),
			}).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var body readyzResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != "ok" {
				t.Errorf("status field = %q, want ok", body.Status)
			}
			if body.Redis != tt.wantRedis {
				t.Errorf("redis field = %q, want %q", body.Redis, tt.wantRedis)
			}
		})
	}
}
