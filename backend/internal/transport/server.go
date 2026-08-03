package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sbidhya/tessera/internal/config"
)

// Server is the HTTP transport layer. It owns the ServeMux and exposes
// lifecycle helpers (Handler, ListenAndServe). Business logic lives in
// engine/room packages — transport only translates HTTP ↔ domain commands,
// per the strict inward-pointing layer rule: engine ← room ← transport.
//
// B0 only exposes GET /healthz. Future blocks will add:
//   - POST /api/matches, GET /api/matches, GET /api/matches/{id}
//   - GET /ws (WebSocket upgrade)
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	mux     *http.ServeMux
	httpSrv *http.Server
}

// New constructs a Server from Config and a structured logger.
// The returned Server is ready to serve but not yet listening.
func New(cfg config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		// Never panic on nil logger — fall back to a no-op logger that discards output.
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	mux := http.NewServeMux()
	s := &Server{
		cfg:    cfg,
		logger: logger,
		mux:    mux,
	}

	// Routes — keep registration centralized so tests can enumerate them.
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Wrap mux with request logging middleware (structured, not fmt.Printf).
	// For B0 this is just the health endpoint, but the middleware will carry
	// forward to all future routes.
	handler := s.loggingMiddleware(mux)

	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        10 * time.Second,
		WriteTimeout:       10 * time.Second,
		IdleTimeout:        60 * time.Second,
	}

	return s
}

// Handler returns the underlying http.Handler (wrapped with middleware).
// Exposed so tests can use httptest without starting a real listener.
func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

// Addr returns the configured listen address.
func (s *Server) Addr() string {
	return s.cfg.Addr
}

// ListenAndServe starts the HTTP server and blocks until context cancellation
// or a listen error. It handles graceful shutdown with a 5s timeout.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", "addr", s.cfg.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.logger.Info("shutting down http server")
		return s.httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// healthResponse is the JSON payload for /healthz.
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealthz is the liveness probe used by load balancers, k8s, and the
// mobile client's server-status check. It must be cheap, synchronous, and
// never depend on DB or room state — if healthz fails, the process is
// considered down regardless of game state.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Only GET and HEAD are allowed. HEAD is useful for cheap probes that
	// only need the status code. Any other method returns 405 to surface
	// misconfigured clients early.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Add no-cache headers so probes always hit the server.
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: "ok"})
}

// loggingMiddleware logs every request at debug level with latency. It uses
// slog so log aggregation can filter by method/path/status without parsing.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		// Health probes are frequent; log them at Debug to avoid spamming Info.
		// Future game routes will log at Info.
		level := slog.LevelDebug
		if rw.status >= 500 {
			level = slog.LevelError
		} else if rw.status >= 400 {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", duration.Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// responseWriter captures the status code for logging.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
