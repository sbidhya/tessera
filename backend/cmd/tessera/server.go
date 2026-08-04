package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
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
