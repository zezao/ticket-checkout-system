package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGenerated(t *testing.T) {
	t.Parallel()

	var seen string
	handler := withRequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	id := rec.Header().Get(requestIDHeader)
	if id == "" {
		t.Fatal("X-Request-ID header is empty")
	}
	if seen != id {
		t.Errorf("context request id = %q, header = %q", seen, id)
	}
}

func TestRequestIDPreserved(t *testing.T) {
	t.Parallel()

	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(RequestID(r.Context())))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "client-request-id")

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(requestIDHeader); got != "client-request-id" {
		t.Errorf("header = %q, want %q", got, "client-request-id")
	}
	if got := rec.Body.String(); got != "client-request-id" {
		t.Errorf("body = %q, want %q", got, "client-request-id")
	}
}

func TestRequestIDReplacedWhenInvalid(t *testing.T) {
	t.Parallel()

	handler := withRequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, strings.Repeat("x", maxRequestIDLen+1))

	handler.ServeHTTP(rec, req)

	got := rec.Header().Get(requestIDHeader)
	if got == "" {
		t.Fatal("X-Request-ID header is empty")
	}
	if got == req.Header.Get(requestIDHeader) {
		t.Error("invalid request id was preserved")
	}
}

func TestRecoveryReturnsErrorEnvelope(t *testing.T) {
	t.Parallel()

	logger := testLogger()
	handler := withRequestID(withLogging(logger, withRecovery(logger,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}),
	)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want %q", body.Error.Code, "internal_error")
	}
}

func TestLoggingIncludesRequestContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := withRequestID(withLogging(logger,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/thing", nil)
	req.Header.Set(requestIDHeader, "client-request-id")

	handler.ServeHTTP(rec, req)

	logged := buf.String()
	for _, want := range []string{
		`"request_id":"client-request-id"`,
		`"method":"GET"`,
		`"path":"/thing"`,
		`"status":204`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line %q does not contain %q", logged, want)
		}
	}
}
