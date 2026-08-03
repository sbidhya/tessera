package config_test

import (
	"math/rand"
	"os"
	"testing"

	"github.com/sbidhya/tessera/internal/config"
)

func TestDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.Addr == "" {
		t.Fatal("Default Addr must not be empty")
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	// Seed should be non-zero (time.Now().UnixNano())
	if cfg.Seed == 0 {
		t.Error("Seed must not be zero")
	}
}

func TestFromEnv_Overrides(t *testing.T) {
	t.Setenv("TESSERA_ADDR", ":9090")
	t.Setenv("TESSERA_LOG_LEVEL", "debug")
	t.Setenv("TESSERA_LOG_FORMAT", "text")
	t.Setenv("TESSERA_SEED", "42")

	cfg := config.FromEnv()

	if cfg.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", cfg.Addr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
	if cfg.Seed != 42 {
		t.Errorf("Seed = %d, want 42", cfg.Seed)
	}
}

func TestFromEnv_DefaultsWhenUnset(t *testing.T) {
	// Ensure env is clean
	os.Unsetenv("TESSERA_ADDR")
	os.Unsetenv("TESSERA_LOG_LEVEL")
	os.Unsetenv("TESSERA_LOG_FORMAT")
	os.Unsetenv("TESSERA_SEED")

	cfg := config.FromEnv()
	def := config.Default()

	// Addr/LogLevel/LogFormat should match defaults
	if cfg.Addr != def.Addr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, def.Addr)
	}
	if cfg.LogLevel != def.LogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, def.LogLevel)
	}
	if cfg.LogFormat != def.LogFormat {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, def.LogFormat)
	}
}

func TestFromEnv_InvalidSeedIgnored(t *testing.T) {
	t.Setenv("TESSERA_SEED", "not-a-number")
	// Capture default seed before FromEnv
	def := config.Default()
	cfg := config.FromEnv()
	// Invalid seed should leave seed as whatever Default produced (non-zero, but we can't assert exact)
	// Just ensure it didn't become 0 and didn't panic.
	if cfg.Seed == 0 && def.Seed != 0 {
		// If default also happened to be 0 (extremely unlikely), skip
		t.Error("Seed became 0 after invalid env value; expected fallback to default")
	}
}

func TestRNG_Deterministic(t *testing.T) {
	cfg := config.Config{Seed: 12345}

	rng1 := cfg.RNG()
	rng2 := cfg.RNG()

	// Two RNGs from same seed must produce identical sequences.
	const n = 10
	for i := 0; i < n; i++ {
		a := rng1.Int63()
		b := rng2.Int63()
		if a != b {
			t.Fatalf("determinism failed at iteration %d: %d != %d", i, a, b)
		}
	}
}

func TestRNG_DifferentSeedsDiverge(t *testing.T) {
	cfgA := config.Config{Seed: 1}
	cfgB := config.Config{Seed: 2}

	rngA := cfgA.RNG()
	rngB := cfgB.RNG()

	// With different seeds, first value should differ (probabilistically 1-1/2^63)
	if rngA.Int63() == rngB.Int63() {
		t.Error("different seeds produced same first Int63; possible but extremely unlikely")
	}
}

func TestNewRNG_Deterministic(t *testing.T) {
	rng1 := config.NewRNG(999)
	rng2 := config.NewRNG(999)

	for i := 0; i < 5; i++ {
		if rng1.Intn(1000) != rng2.Intn(1000) {
			t.Fatalf("NewRNG not deterministic at iteration %d", i)
		}
	}
}

func TestRNG_ShuffleDeterministic(t *testing.T) {
	// Simulate deck shuffle determinism: shuffling with same seed yields same order.
	shuffle := func(seed int64) []int {
		r := rand.New(rand.NewSource(seed)) // reference for sanity
		_ = r
		cfg := config.Config{Seed: seed}
		rng := cfg.RNG()
		deck := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		rng.Shuffle(len(deck), func(i, j int) {
			deck[i], deck[j] = deck[j], deck[i]
		})
		return deck
	}

	a := shuffle(42)
	b := shuffle(42)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("shuffle not deterministic: %v vs %v", a, b)
		}
	}

	c := shuffle(43)
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different seeds produced identical shuffle; unexpected")
	}
}
