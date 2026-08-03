package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sbidhya/tessera/internal/config"
	"github.com/sbidhya/tessera/internal/logger"
	"github.com/sbidhya/tessera/internal/transport"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Config: env overrides defaults. A single Seed controls every RNG in the
	// process — see config.Config.RNG() for why.
	cfg := config.FromEnv()

	// Structured logger: JSON to stdout, level from config.
	// Using slog (stdlib) keeps the dependency graph minimal for B0.
	log := logger.New(cfg.LogLevel, cfg.LogFormat, os.Stdout)
	slog.SetDefault(log) // so any library using slog.Default() inherits our handler

	log.Info("starting tessera",
		"addr", cfg.Addr,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"seed", cfg.Seed,
	)

	// Demonstrate seeded RNG is wired: log the first value so an operator can
	// verify determinism by fixing TESSERA_SEED.
	rng := cfg.RNG()
	log.Debug("rng seeded", "first_int63", rng.Int63())

	srv := transport.New(cfg, log)

	// Context that cancels on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.ListenAndServe(ctx); err != nil {
		log.Error("server stopped with error", "error", err)
		return err
	}

	log.Info("server stopped gracefully")
	return nil
}
