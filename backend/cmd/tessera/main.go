// Command tessera is the game backend entrypoint.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sbidhya/tessera/backend/internal/auth"
	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/match"
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
	coldStore, err := store.Open(cfg.DBPath, journal, logger, store.Options{
		BatchSize:     cfg.StoreBatchSize,
		FlushInterval: cfg.StoreFlushInterval,
	})
	if err != nil {
		_ = journal.Close()
		logger.Error("open SQLite store", "err", err)
		return err
	}

	manager, err := room.NewPersistentManager(logger, cfg.NewRand, journal, coldStore)
	if err != nil {
		_ = coldStore.Close()
		_ = journal.Close()
		logger.Error("recover rooms", "err", err)
		return err
	}
	// Retention: a finished, archived match stays live for the grace window
	// so an immediate reconnect still gets its final snapshot, then Sweep
	// evicts the actor (goroutine, idempotency map, event history) and the
	// hub. The store notifies the manager after each SQLite commit — the
	// enqueue alone does not make a room eligible.
	manager.SetRetention(cfg.RoomRetention, time.Now)
	coldStore.SetOnArchived(manager.NotifyArchived)

	// B6 lobby: anonymous identities, matchmaking, and presence. The secret
	// defaults to a seed-derived value so a dev restart keeps every issued
	// token valid; production must set TESSERA_AUTH_SECRET explicitly.
	secret := cfg.AuthSecret
	if secret == "" {
		secretRand := cfg.NewRand("auth-secret")
		var raw [32]byte
		for i := range raw {
			raw[i] = byte(secretRand.UintN(256))
		}
		secret = hex.EncodeToString(raw[:])
		logger.Warn("TESSERA_AUTH_SECRET not set; using seed-derived dev secret — set a real secret in production")
	}
	authenticator := auth.New([]byte(secret), cfg.NewRand("player-ids"))
	matchmaker := match.NewMatchmaker(logger, manager)
	presence := match.NewPresence()

	api := transport.NewWithDeps(manager, logger, transport.Deps{
		Auth:       authenticator,
		Matchmaker: matchmaker,
		Presence:   presence,
		IsArchived: func(ctx context.Context, id string) (bool, error) {
			_, err := coldStore.Match(ctx, id)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		},
	})
	// Evicting a room also retires its hub (P0-1 coordination): without this
	// the hub goroutine and its client maps would leak alongside the room.
	manager.SetEvictHook(api.CloseHub)
	logger.Info("room retention configured", "retention", cfg.RoomRetention.String())

	// Periodically evict finished, archived matches past their grace window.
	// Sweep itself is cheap (one snapshot per live room) and idempotent, so
	// a fixed cadence derived from the retention is enough; tests drive
	// Sweep explicitly with a fake clock instead of this ticker.
	sweepStop := make(chan struct{})
	defer close(sweepStop)
	go retentionSweeper(manager, cfg.RoomRetention, sweepStop)
	defer func() {
		api.Close()
		// Unblock matchmaking waiters before rooms go away: a waiter released
		// by Close during Shutdown would otherwise race Manager.Create
		// against Shutdown and fail confusingly.
		matchmaker.Close()
		manager.Shutdown()
		if err := coldStore.Close(); err != nil {
			logger.Error("close SQLite store", "err", err)
		}
		if err := journal.Close(); err != nil {
			logger.Error("close WAL", "err", err)
		}
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

// retentionSweeper calls Manager.Sweep on a cadence derived from the grace
// window: often enough that an expired room does not linger long, seldom
// enough that idle servers do near-zero work.
func retentionSweeper(manager *room.Manager, retention time.Duration, stop <-chan struct{}) {
	interval := retention / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			manager.Sweep()
		case <-stop:
			return
		}
	}
}
