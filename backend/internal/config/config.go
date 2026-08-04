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
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"sync/atomic"
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

	// rngState tracks how many streams have been handed out. Each call to
	// NewRand consumes one sequence number, so two callers never receive the
	// same PCG seeds. The state is a pointer so that copies of Config (e.g.
	// cfg2 := cfg) share the same counter and remain independent, while
	// distinct Config values (different Seed or freshly constructed) start
	// from 0 and are therefore reproducible across runs.
	rngState *streamState
}

type streamState struct {
	seq atomic.Uint64
}

// Default returns the built-in configuration used when no environment
// overrides are present.
func Default() Config {
	return Config{
		Addr:     ":8080",
		Seed:     1,
		LogLevel: slog.LevelInfo,
		rngState: &streamState{},
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
// Each call returns an independent generator: the internal sequence counter is
// advanced atomically and mixed with Seed to produce distinct PCG seeds. Two
// successive calls on the same Config therefore diverge, fixing the hazard
// where two subsystems both calling NewRand would otherwise shuffle identically.
//
// Reproducibility is preserved: a new Config with the same Seed starts its
// counter at 0, so the Nth call on one process yields byte-identical output to
// the Nth call on another process with the same Seed. For cases where call
// order is not stable (e.g. rooms created concurrently), prefer NewRandFor
// which derives the stream from a name rather than call order.
func (c *Config) NewRand() *rand.Rand {
	if c.rngState == nil {
		c.rngState = &streamState{}
	}
	seq := c.rngState.seq.Add(1) - 1
	s1, s2 := derivePCGSeeds(c.Seed, seq)
	return rand.New(rand.NewPCG(s1, s2))
}

// NewRandFor returns a deterministic random source for a named consumer
// (e.g. a room ID). The stream depends only on Seed and name, not on how many
// other streams have been created, so it is stable regardless of call order
// and remains reproducible across runs.
func (c *Config) NewRandFor(name string) *rand.Rand {
	// FNV-1a is fast, deterministic, and sufficient to separate names.
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(c.Seed))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(name))
	s1 := splitMix64(h.Sum64())
	s2 := splitMix64(s1 ^ 0x9e3779b97f4a7c15)
	// PCG's second word is the stream; avoid the single degenerate zero case.
	if s2 == 0 {
		s2 = 0x9e3779b97f4a7c15
	}
	return rand.New(rand.NewPCG(s1, s2))
}

// RNG is an alias for NewRand kept for compatibility with earlier call sites
// that used Config.RNG(). It has the same per-call independent, reproducible
// semantics.
func (c *Config) RNG() *rand.Rand { return c.NewRand() }

// RNGFor is an alias for NewRandFor.
func (c *Config) RNGFor(name string) *rand.Rand { return c.NewRandFor(name) }

// derivePCGSeeds mixes Seed and seq into two well-distributed PCG words.
func derivePCGSeeds(seed int64, seq uint64) (uint64, uint64) {
	s := uint64(seed)
	// Mix seq into both words with different constants so successive seqs
	// diverge in both PCG inputs.
	s1 := splitMix64(s + seq*0x9e3779b97f4a7c15)
	s2 := splitMix64(s ^ (seq*0xbf58476d1ce4e5b9 + 0x94d049bb133111eb))
	if s2 == 0 {
		s2 = 0x9e3779b97f4a7c15
	}
	return s1, s2
}

// splitMix64 is the SplitMix64 mixer (Steele et al.). It avalanches a single
// uint64 into a well-distributed output, ideal for turning a counter into
// unrelated seeds.
func splitMix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	z := x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
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
