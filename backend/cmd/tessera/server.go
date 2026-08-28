package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sbidhya/tessera/backend/internal/store"
)

// buildInfo is reported by /healthz. It is intentionally tiny for B0; later
// blocks can extend it with match counts, WAL status, etc.
type healthResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

// newRouter constructs the HTTP handler for the backend.
//
// It lives in its own function (rather than being inlined in main) so tests can
// exercise the exact routing/handlers the server uses without binding a socket.
// start marks process boot so /healthz can report uptime.
//
// If cold is non-nil, B5 history and stats routes are mounted at /v1/history and
// /v1/players/{id}/stats. They are served from SQLite and are read-only; the
// history handler respects ?limit&offset pagination.
func newRouter(logger *slog.Logger, start time.Time, now func() time.Time, api http.Handler) http.Handler {
	return newRouterWithStore(logger, start, now, api, nil)
}

func newRouterWithStore(logger *slog.Logger, start time.Time, now func() time.Time, api http.Handler, cold *store.Store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		resp := healthResponse{
			Status: "ok",
			Uptime: now().Sub(start).Round(time.Millisecond).String(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.Error("healthz: encode response", "err", err)
		}
	})
	if cold != nil {
		mux.HandleFunc("GET /v1/history", historyHandler(logger, cold))
		mux.HandleFunc("GET /v1/players/{playerID}/stats", playerStatsHandler(logger, cold))
	}
	if api != nil {
		mux.Handle("/v1/", api)
	}

	return requestLogger(logger, mux)
}

func historyHandler(logger *slog.Logger, cold *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limitStr := r.URL.Query().Get("limit")
		offsetStr := r.URL.Query().Get("offset")
		limit := 50
		offset := 0
		if limitStr != "" {
			v, err := strconv.Atoi(limitStr)
			if err != nil || v < 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
				return
			}
			limit = v
		}
		if offsetStr != "" {
			v, err := strconv.Atoi(offsetStr)
			if err != nil || v < 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
				return
			}
			offset = v
		}
		records, err := cold.ListHistory(r.Context(), limit, offset)
		if err != nil {
			logger.Error("history: list", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"matches": records})
	}
}

func playerStatsHandler(logger *slog.Logger, cold *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		playerID := r.PathValue("playerID")
		if playerID == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_player_id", "player_id is required")
			return
		}
		stats, err := cold.GetPlayerStats(r.Context(), playerID)
		if err != nil {
			logger.Error("stats: get", "player", playerID, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"stats": stats})
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// context helper for tests that need background.
var _ = context.Background

// requestLogger wraps a handler with structured access logging. It records the
// method, path, status, and duration of every request at debug level so normal
// operation stays quiet but tracing is one env var away.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).String(),
		)
	})
}

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap lets net/http.ResponseController reach optional interfaces on the
// underlying writer (notably Hijacker for the WebSocket upgrade).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
