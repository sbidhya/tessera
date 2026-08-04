// Command tessera is the game backend entrypoint.
//
// B0 scope: load config, set up structured logging and a seeded RNG, and serve
// a /healthz endpoint with graceful shutdown. Later blocks add the room manager,
// WebSocket transport, WAL, and SQLite tiers behind this same process.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
)

func main() {
	if err := run(); err != nil {
		// run() already logged the detail; exit non-zero for supervisors.
		os.Exit(1)
	}
}

// run wires the process together and blocks until it is signalled to stop.
// It returns an error instead of calling os.Exit so it stays testable and so
// deferred cleanup runs.
func run() error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		// No logger yet; config failed to load. Fall back to a default one.
		config.Default().Logger().Error("load config", "err", err)
		return err
	}

	logger := cfg.Logger()

	// Confirm the seeded RNG is wired; a later block will hand this to the deck
	// shuffler. Logging one draw at debug level makes the seed visible in dev.
	rng := cfg.NewRand()
	logger.Debug("rng initialised", "seed", cfg.Seed, "sample", rng.Uint64())

	start := time.Now()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newRouter(logger, start, time.Now),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Serve in the background so main can wait on shutdown signals.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Wait for SIGINT/SIGTERM or a fatal serve error.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("http server failed", "err", err)
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting new connections and let in-flight ones
	// finish, up to a deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		return err
	}
	logger.Info("shutdown complete")
	return nil
}
