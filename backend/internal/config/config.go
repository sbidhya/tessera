// Package config holds process-wide configuration for the Tessera backend.
//
// It is deliberately a leaf package: it imports nothing from the rest of the
// application, so any layer (engine, room, transport, ...) may depend on it
// without creating an import cycle or violating the inward-pointing layering.
//
// The most important thing here is the seeded RNG. Every source of randomness
// in the game (notably deck shuffles) must be derived from Config.NewRand so
// that a given Seed produces an identical game. That makes tests deterministic
// and makes bugs reproducible from a single integer.
package config

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
)

// Config is the fully-resolved configuration for one process.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string
	// Seed seeds all randomness. Two processes with the same Seed shuffle
	// identically. Override for chaos; pin for reproducible tests.
	Seed int64
	// LogLevel is the minimum slog level to emit: debug, info, warn, error.
	LogLevel slog.Level
}

// Default returns the built-in configuration used when no environment
// overrides are present.
func Default() Config {
	return Config{
		Addr:     ":8080",
		Seed:     1,
		LogLevel: slog.LevelInfo,
	}
}

// Load builds a Config from Default(), overriding fields from environment
// variables when present:
//
//	TESSERA_ADDR       -> Addr       (string)
//	TESSERA_SEED       -> Seed       (int64)
//	TESSERA_LOG_LEVEL  -> LogLevel   (debug|info|warn|error)
//
// It returns an error rather than silently falling back so a typo in a
// deployment env var fails loudly instead of running with a surprising value.
func Load(getenv func(string) string) (Config, error) {
	cfg := Default()

	if v := getenv("TESSERA_ADDR"); v != "" {
		cfg.Addr = v
	}

	if v := getenv("TESSERA_SEED"); v != "" {
		seed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TESSERA_SEED %q: %w", v, err)
		}
		cfg.Seed = seed
	}

	if v := getenv("TESSERA_LOG_LEVEL"); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			return Config{}, err
		}
		cfg.LogLevel = lvl
	}

	return cfg, nil
}

// LoadFromEnv is a convenience wrapper around Load using os.Getenv.
func LoadFromEnv() (Config, error) {
	return Load(os.Getenv)
}

// NewRand returns a fresh, deterministic random source derived from Seed.
//
// Each call returns an independent generator positioned at the same starting
// point, so callers that each want the "same" stream get it. Because it is
// seeded from a plain int64, an entire game's shuffle is reproducible from that
// one number.
func (c Config) NewRand() *rand.Rand {
	// PCG takes two 64-bit words. We derive the second word from the seed with
	// a fixed transform so a single int64 fully determines the stream while
	// still exercising both PCG inputs.
	s := uint64(c.Seed)
	return rand.New(rand.NewPCG(s, s^0x9e3779b97f4a7c15))
}

// Logger builds a JSON structured logger at the configured level, writing to
// the process's stderr. Kept here so every entrypoint (server, future CLIs,
// tests) constructs logging identically.
func (c Config) Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: c.LogLevel,
	}))
}

func parseLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	// slog.Level.UnmarshalText understands "debug"/"info"/"warn"/"error"
	// (case-insensitive) as well as "INFO+2" style offsets.
	if err := lvl.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("config: invalid TESSERA_LOG_LEVEL %q: %w", s, err)
	}
	return lvl, nil
}
