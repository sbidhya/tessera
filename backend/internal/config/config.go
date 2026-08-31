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
//
// NewRand takes a stream name so that independent subsystems — each room, the
// deck, matchmaking, ... — get statistically independent generators that never
// alias onto one another, while the whole process stays reproducible from the
// single Seed. See NewRand for the derivation.
package config

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"time"
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
	// WALDir is the directory containing one append-only log per match.
	WALDir string
	// WALSync controls acknowledgement durability: "always" fsyncs every
	// accepted event, while "never" relies on the operating system's flushing.
	WALSync string
	// DBPath is the SQLite database used for finished-match history and stats.
	DBPath string
	// StoreBatchSize is the maximum number of finished matches committed in one
	// SQLite transaction.
	StoreBatchSize int
	// StoreFlushInterval bounds how long a partial write-behind batch waits.
	StoreFlushInterval time.Duration
	// AuthSecret is the HMAC key that signs anonymous identity tokens
	// (internal/auth). Empty selects the deterministic dev default: a
	// 32-byte value drawn from the "auth-secret" RNG stream, so the same
	// Seed keeps issuing verifiable tokens across restarts. Anyone holding
	// the secret can mint arbitrary identities, so production deployments
	// MUST set TESSERA_AUTH_SECRET to an unpredictable value; the process
	// logs a warning when it falls back to the seed-derived default.
	AuthSecret string
}

// Default returns the built-in configuration used when no environment
// overrides are present.
func Default() Config {
	return Config{
		Addr:               ":8080",
		Seed:               1,
		LogLevel:           slog.LevelInfo,
		WALDir:             "data/wal",
		WALSync:            "always",
		DBPath:             "data/tessera.db",
		StoreBatchSize:     16,
		StoreFlushInterval: time.Second,
	}
}

// Load builds a Config from Default(), overriding fields from environment
// variables when present:
//
//	TESSERA_ADDR       -> Addr       (string)
//	TESSERA_SEED       -> Seed       (int64)
//	TESSERA_LOG_LEVEL  -> LogLevel   (debug|info|warn|error)
//	TESSERA_WAL_DIR    -> WALDir     (directory path)
//	TESSERA_WAL_SYNC   -> WALSync    (always|never)
//	TESSERA_DB_PATH    -> DBPath     (SQLite file path)
//	TESSERA_STORE_BATCH_SIZE -> StoreBatchSize (positive integer)
//	TESSERA_STORE_FLUSH_INTERVAL -> StoreFlushInterval (Go duration)
//	TESSERA_AUTH_SECRET -> AuthSecret (HMAC key for identity tokens; empty = seed-derived dev default)
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

	if v := getenv("TESSERA_WAL_DIR"); v != "" {
		cfg.WALDir = v
	}

	if v := getenv("TESSERA_WAL_SYNC"); v != "" {
		switch v {
		case "always", "never":
			cfg.WALSync = v
		default:
			return Config{}, fmt.Errorf("config: invalid TESSERA_WAL_SYNC %q (want always or never)", v)
		}
	}

	if v := getenv("TESSERA_DB_PATH"); v != "" {
		cfg.DBPath = v
	}

	if v := getenv("TESSERA_STORE_BATCH_SIZE"); v != "" {
		size, err := strconv.Atoi(v)
		if err != nil || size < 1 {
			return Config{}, fmt.Errorf("config: invalid TESSERA_STORE_BATCH_SIZE %q (want a positive integer)", v)
		}
		cfg.StoreBatchSize = size
	}

	if v := getenv("TESSERA_STORE_FLUSH_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil || interval <= 0 {
			return Config{}, fmt.Errorf("config: invalid TESSERA_STORE_FLUSH_INTERVAL %q (want a positive duration)", v)
		}
		cfg.StoreFlushInterval = interval
	}

	if v := getenv("TESSERA_AUTH_SECRET"); v != "" {
		cfg.AuthSecret = v
	}

	return cfg, nil
}

// LoadFromEnv is a convenience wrapper around Load using os.Getenv.
func LoadFromEnv() (Config, error) {
	return Load(os.Getenv)
}

// NewRand returns a fresh, deterministic random source for the named stream.
//
// Streams are keyed by name: NewRand("room-7") and NewRand("deck") return
// independent generators, so two subsystems drawing concurrently never receive
// the same sequence — the correctness hazard that a single shared stream would
// create. Yet everything is derived from the single Seed, so:
//
//   - the same (Seed, name) yields a byte-identical sequence on every run, and
//   - the mapping is order-independent: which subsystem calls first does not
//     change any stream, so a whole game is reproducible from one integer.
//
// Derivation: hash (Seed, name) into one 64-bit value, then expand it into
// PCG's two state words with two splitmix64 steps. splitmix64 has good
// avalanche, so even near-identical names ("room-1" vs "room-2") map to
// well-separated, statistically independent PCG states.
func (c Config) NewRand(stream string) *rand.Rand {
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(c.Seed))
	_, _ = h.Write(seed[:])
	_, _ = io.WriteString(h, stream)
	mixed := h.Sum64()

	return rand.New(rand.NewPCG(
		splitmix64(mixed),
		splitmix64(mixed+0x9e3779b97f4a7c15),
	))
}

// splitmix64 is the SplitMix64 finalizer. It maps each 64-bit input to a
// well-distributed 64-bit output, giving each named stream a state that is
// decorrelated from its neighbours.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
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
