package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/transport"
)

// discardLogger returns a logger that throws output away, keeping tests quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestHealthzReturns200(t *testing.T) {
	start := time.Unix(1000, 0)
	// now() is 2s after start so uptime is deterministic and non-zero.
	now := func() time.Time { return start.Add(2 * time.Second) }

	router := newRouter(discardLogger(), start, now, nil)

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
	router := newRouter(discardLogger(), time.Unix(0, 0), time.Now, nil)

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
	router := newRouter(discardLogger(), time.Unix(0, 0), time.Now, nil)

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// The access logger wraps ResponseWriter. This integration check ensures its
// Unwrap method preserves the Hijacker capability needed for a real WebSocket
// upgrade through the exact router used by main.
func TestRouterPreservesWebSocketUpgrade(t *testing.T) {
	logger := discardLogger()
	cfg := config.Config{Seed: 1}
	manager := room.NewManager(logger, cfg.NewRand)
	api := transport.New(manager, logger)
	t.Cleanup(func() {
		api.Close()
		manager.Shutdown()
	})
	match, err := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("create match: %v", err)
	}

	server := httptest.NewServer(newRouter(logger, time.Now(), time.Now, api.Handler()))
	t.Cleanup(server.Close)
	wsURL := "ws" + server.URL[len("http"):] + "/v1/matches/" + match.ID() + "/ws?player_id=alice"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial through app router: %v", err)
	}
	defer conn.CloseNow()

	var envelope struct {
		Type string `json:"type"`
	}
	if err := wsjson.Read(ctx, conn, &envelope); err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	if envelope.Type != "state" {
		t.Fatalf("initial envelope type = %q, want state", envelope.Type)
	}
}
