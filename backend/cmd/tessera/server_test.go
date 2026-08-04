package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// discardLogger returns a logger that throws output away, keeping tests quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestHealthzReturns200(t *testing.T) {
	start := time.Unix(1000, 0)
	// now() is 2s after start so uptime is deterministic and non-zero.
	now := func() time.Time { return start.Add(2 * time.Second) }

	router := newRouter(discardLogger(), start, now)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status field = %q, want ok", resp.Status)
	}
	if resp.Uptime != "2s" {
		t.Errorf("uptime = %q, want 2s", resp.Uptime)
	}
}

func TestHealthzRejectsNonGET(t *testing.T) {
	router := newRouter(discardLogger(), time.Unix(0, 0), time.Now)

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// The method-specific route pattern ("GET /healthz") makes the mux return
	// 405 for other methods automatically.
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	router := newRouter(discardLogger(), time.Unix(0, 0), time.Now)

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
