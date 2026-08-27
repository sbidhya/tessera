// Command tessera is the game backend entrypoint.
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
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/transport"
	"github.com/sbidhya/tessera/backend/internal/wal"
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

	// Logging from a dedicated probe stream makes the configured seed observable
	// without perturbing room ids or any game's board/deck streams.
	rng := cfg.NewRand("startup-probe")
	logger.Debug("rng initialised", "seed", cfg.Seed, "sample", rng.Uint64())

	manager := room.NewManager(logger, cfg.NewRand)

	// Durability (B4): if a WAL directory is configured, replay the log to
	// rebuild every match before serving, then wire the store for future
	// write-ahead. The replay is idempotent — duplicate entries are deduped
	// via move_id — so a crash between WAL and SQLite (B5) still recovers.
	var walStore *wal.Store
	if cfg.WALDir != "" {
		policy, err := wal.ParseSyncPolicy(cfg.WALSync)
		if err != nil {
			logger.Error("invalid wal sync policy", "err", err)
			return err
		}
		walStore, err = wal.New(cfg.WALDir, policy)
		if err != nil {
			logger.Error("open wal", "dir", cfg.WALDir, "err", err)
			return err
		}
		if err := walStore.Replay(manager); err != nil {
			logger.Error("wal replay failed", "err", err)
			return err
		}
		if n := len(manager.List()); n > 0 {
			logger.Info("wal replay complete", "rooms", n, "dir", cfg.WALDir)
		}
	} else {
		logger.Info("wal disabled (no dir configured)")
	}
	api := transport.New(manager, logger)
	defer func() {
		api.Close()
		manager.Shutdown()
	}()

	start := time.Now()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newRouter(logger, start, time.Now, api.Handler()),
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
	// net/http does not wait for hijacked WebSocket connections. Close the
	// transport explicitly so every socket and hub exits before rooms do.
	api.Close()
	logger.Info("shutdown complete")
	return nil
}
