package config

import (
	"log/slog"
	"testing"
)

// envFunc builds a getenv-style lookup from a map for hermetic tests.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.Seed != 1 {
		t.Errorf("Seed = %d, want 1", cfg.Seed)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want Info", cfg.LogLevel)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"TESSERA_ADDR":      ":9999",
		"TESSERA_SEED":      "42",
		"TESSERA_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", cfg.Addr)
	}
	if cfg.Seed != 42 {
		t.Errorf("Seed = %d, want 42", cfg.Seed)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want Debug", cfg.LogLevel)
	}
}

func TestLoadEmptyKeepsDefaults(t *testing.T) {
	cfg, err := Load(envFunc(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg != Default() {
		t.Errorf("Load with no env = %+v, want %+v", cfg, Default())
	}
}

func TestLoadInvalidSeed(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"TESSERA_SEED": "not-a-number"}))
	if err == nil {
		t.Fatal("expected error for invalid seed, got nil")
	}
}

func TestLoadInvalidLogLevel(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"TESSERA_LOG_LEVEL": "chatty"}))
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
}

// drawN pulls n consecutive uint64s from r into a slice for easy comparison.
func drawN(r interface{ Uint64() uint64 }, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = r.Uint64()
	}
	return out
}

// TestNewRandSameStreamDeterministic proves the within-run half of the
// reproducibility guarantee: the same (Seed, name) yields the same sequence,
// so a given consumer always draws identically.
func TestNewRandSameStreamDeterministic(t *testing.T) {
	cfg := Config{Seed: 12345}

	r1 := cfg.NewRand("deck")
	r2 := cfg.NewRand("deck")

	for i := 0; i < 100; i++ {
		a, b := r1.Uint64(), r2.Uint64()
		if a != b {
			t.Fatalf("draw %d: %d != %d — same (seed,name) must yield same stream", i, a, b)
		}
	}
}

// TestNewRandStreamsDiverge proves the independence property: two named streams
// from the *same* Config must diverge from each other. This is the bug the
// per-stream derivation fixes — previously every stream aliased onto one shared
// sequence, so two subsystems drawing at once got identical "random" values.
func TestNewRandStreamsDiverge(t *testing.T) {
	cfg := Config{Seed: 12345}

	// Compare several distinct names pairwise; none may share a sequence.
	names := []string{"deck", "room-1", "room-2", "matchmaking"}
	streams := make([][]uint64, len(names))
	for i, n := range names {
		streams[i] = drawN(cfg.NewRand(n), 8)
	}

	equal := func(a, b []uint64) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if equal(streams[i], streams[j]) {
				t.Fatalf("streams %q and %q produced identical sequences — streams are not independent",
					names[i], names[j])
			}
		}
	}
}

// TestNewRandReproducibleAcrossRuns proves the cross-run property with golden
// values: for a fixed (Seed, name) the first draws are byte-identical on every
// run of the binary. If this test ever fails, the RNG derivation changed and
// every previously-recorded game replay is invalidated — treat it as a
// deliberate, breaking decision, not an incidental edit.
func TestNewRandReproducibleAcrossRuns(t *testing.T) {
	cfg := Config{Seed: 12345}

	got := drawN(cfg.NewRand("deck"), 4)
	want := []uint64{
		0x5f2b44d19c0bf12e, 0x845ea453f3d76917,
		0xb1d8bea93f48aea4, 0x55da41b36d863ee7,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("draw %d = %#016x, want %#016x — RNG stream drifted across runs", i, got[i], want[i])
		}
	}
}

// TestNewRandDiffersBySeed confirms the Seed still selects the whole family of
// streams: the same name under two different seeds diverges.
func TestNewRandDiffersBySeed(t *testing.T) {
	a := Config{Seed: 1}.NewRand("deck")
	b := Config{Seed: 2}.NewRand("deck")

	// Extremely unlikely that two different seeds match on the first draw.
	if a.Uint64() == b.Uint64() {
		t.Fatal("different seeds produced identical first draw for the same stream")
	}
}
