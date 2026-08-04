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

// TestNewRandDeterministic is the load-bearing test for the whole project's
// reproducibility guarantee: the same seed must produce the same stream.
func TestNewRandDeterministic(t *testing.T) {
	cfg := Config{Seed: 12345}

	r1 := cfg.NewRand()
	r2 := cfg.NewRand()

	for i := 0; i < 100; i++ {
		a, b := r1.Uint64(), r2.Uint64()
		if a != b {
			t.Fatalf("draw %d: %d != %d — same seed must yield same stream", i, a, b)
		}
	}
}

func TestNewRandDiffersBySeed(t *testing.T) {
	a := Config{Seed: 1}.NewRand()
	b := Config{Seed: 2}.NewRand()

	// Extremely unlikely that two different seeds match on the first draw.
	if a.Uint64() == b.Uint64() {
		t.Fatal("different seeds produced identical first draw")
	}
}
