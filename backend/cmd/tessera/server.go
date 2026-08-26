package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/transport"
)

// buildInfo is reported by /healthz. It is intentionally tiny for B0; later
// blocks can extend it with match counts, WAL status, etc.
type healthResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

// newRouter constructs the HTTP handler for the backend in the B0 era.
//
// It is retained so existing TestHealthz* tests keep exercising GET /healthz
// without a room manager. New code should use newHandler, which exposes the
// full REST + WebSocket surface via transport.Server.
func newRouter(logger *slog.Logger, start time.Time, now func() time.Time) http.Handler {
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

	return requestLogger(logger, mux)
}

// newHandler builds the production HTTP handler with the full B3 surface
// (healthz + REST + WebSocket) backed by mgr. Tests that need the real
// server should call this instead of newRouter.
func newHandler(mgr *room.Manager, logger *slog.Logger, start time.Time, now func() time.Time) http.Handler {
	return transport.New(mgr, logger, start, now).Handler()
}

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

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("server: ResponseWriter does not implement http.Hijacker")
}

func (w *statusWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
