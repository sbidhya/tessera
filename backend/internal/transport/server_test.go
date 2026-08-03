package transport_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sbidhya/tessera/internal/config"
	"github.com/sbidhya/tessera/internal/transport"
)

// newTestServer builds a Server with a discard logger for tests.
func newTestServer() *transport.Server {
	cfg := config.Config{
		Addr:      ":0",
		LogLevel:  "error", // silence logs in tests
		LogFormat: "json",
		Seed:      42,
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	return transport.New(cfg, logger)
}

func TestHealthz_GET_Returns200AndJSON(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v; body=%q", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestHealthz_HEAD_Returns200NoBody(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD should return empty body, got %q", rec.Body.String())
	}
}

func TestHealthz_MethodNotAllowed(t *testing.T) {
	srv := newTestServer()

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			req := httptest.NewRequest(m, "/healthz", nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: status = %d, want %d", m, rec.Code, http.StatusMethodNotAllowed)
			}
			allow := rec.Header().Get("Allow")
			if allow == "" {
				t.Error("expected Allow header for 405 response")
			}
		})
	}
}

func TestHealthz_NotFoundForUnknownPath(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_NewWithNilLoggerDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New with nil logger panicked: %v", r)
		}
	}()

	cfg := config.Config{Addr: ":0", LogLevel: "info", LogFormat: "json", Seed: 1}
	srv := transport.New(cfg, nil)
	if srv == nil {
		t.Fatal("expected non-nil server")
	}

	// Should still serve healthz
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestServer_Addr(t *testing.T) {
	cfg := config.Config{Addr: ":1234", LogLevel: "info", LogFormat: "json", Seed: 1}
	srv := transport.New(cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if srv.Addr() != ":1234" {
		t.Errorf("Addr() = %q, want :1234", srv.Addr())
	}
}

// TestHealthz_Concurrent ensures the handler is safe for concurrent use.
// This is run under go test -race in CI.
func TestHealthz_Concurrent(t *testing.T) {
	srv := newTestServer()
	handler := srv.Handler()

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent healthz: status = %d, want %d", rec.Code, http.StatusOK)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}
