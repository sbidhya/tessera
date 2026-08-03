package config

import (
	"math/rand"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the tessera backend.
//
// Design choice: keep Config as a plain struct with no global state so it is
// trivial to construct in tests. FromEnv reads environment overrides; Default
// provides sensible local-dev defaults. Seeded RNG is exposed via RNG() so that
// every package needing randomness (shuffles, dice, etc.) can derive a
// deterministic *rand.Rand from the same seed — this is required by the project
// rule "seeded RNG everywhere" for reproducible tests.
//
// Environment variables (all optional):
//
//	TESSERA_ADDR      — HTTP listen address, e.g. ":8080" (default ":8080")
//	TESSERA_LOG_LEVEL — one of debug|info|warn|error (default "info")
//	TESSERA_LOG_FORMAT— json or text (default "json")
//	TESSERA_SEED      — int64 seed for RNG (default: time.Now().UnixNano())
type Config struct {
	Addr      string
	LogLevel  string
	LogFormat string
	Seed      int64
}

// Default returns a Config with production-sensible defaults.
// Seed defaults to time.Now().UnixNano() for non-deterministic runs; tests
// should override Seed explicitly to get reproducibility.
func Default() Config {
	return Config{
		Addr:      ":8080",
		LogLevel:  "info",
		LogFormat: "json",
		Seed:      time.Now().UnixNano(),
	}
}

// FromEnv returns Default() overridden by any recognized environment variables.
// Unknown log levels / formats are not rejected here — validation happens in
// the logger so Config stays a pure data holder.
func FromEnv() Config {
	cfg := Default()

	if v := os.Getenv("TESSERA_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("TESSERA_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("TESSERA_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	if v := os.Getenv("TESSERA_SEED"); v != "" {
		if seed, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Seed = seed
		}
	}

	return cfg
}

// RNG returns a new, isolated *rand.Rand seeded from c.Seed.
//
// Why a method that returns a fresh Rand each call?
//   - Go's global rand is shared and not safe for deterministic tests when
//     multiple packages use it concurrently.
//   - By deriving every shuffle from Config.Seed, a single seed controls all
//     randomness in the process. Tests fix the seed and get byte-for-byte
//     identical shuffles across runs (go test -run TestShuffle).
//   - Callers needing independent streams (e.g. per-room RNG) should call
//     RNG() once and keep the instance; sharing the instance across goroutines
//     requires external synchronization (rand.Rand is not concurrent-safe).
func (c Config) RNG() *rand.Rand {
	return rand.New(rand.NewSource(c.Seed))
}

// NewRNG is a convenience for callers that only have a seed value.
func NewRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}
