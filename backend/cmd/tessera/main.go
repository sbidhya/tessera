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
	"github.com/sbidhya/tessera/backend/internal/store"
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

	syncPolicy, err := wal.ParseSyncPolicy(cfg.WALSync)
	if err != nil {
		logger.Error("configure WAL", "err", err)
		return err
	}
	journal, err := wal.Open(cfg.WALDir, syncPolicy)
	if err != nil {
		logger.Error("open WAL", "err", err)
		return err
	}

	manager, err := room.NewDurableManager(logger, cfg.NewRand, journal)
	if err != nil {
		_ = journal.Close()
		logger.Error("recover rooms", "err", err)
		return err
	}

	// Cold tier: SQLite for finished-match history and player stats. It is the
	// B5 write-behind layer that batches completed games and checkpoints the WAL.
	cold, err := store.Open(cfg.DBPath)
	if err != nil {
		manager.Shutdown()
		_ = journal.Close()
		logger.Error("open cold store", "err", err)
		return err
	}
	flusher := store.NewFlusher(cold, journal, manager, logger)
	flusher.Start()
	// Recovery flush: a crash between WAL append and SQLite may have left finished
	// matches only in the WAL. Flush them now so history is complete before we
	// serve traffic.
	if err := flusher.Flush(context.Background()); err != nil {
		logger.Warn("cold store recovery flush failed", "err", err)
	}

	api := transport.New(manager, logger)
	api.SetFlushHook(flusher.Enqueue)
	defer func() {
		api.Close()
		flusher.Stop()
		if err := cold.Close(); err != nil {
			logger.Error("close cold store", "err", err)
		}
		manager.Shutdown()
		if err := journal.Close(); err != nil {
			logger.Error("close WAL", "err", err)
		}
	}()

	start := time.Now()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newRouterWithStore(logger, start, time.Now, api.Handler(), cold),
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
