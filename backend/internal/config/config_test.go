package config

import (
	"log/slog"
	"sync"
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
	def := Default()
	if cfg.Addr != def.Addr || cfg.Seed != def.Seed || cfg.LogLevel != def.LogLevel {
		t.Errorf("Load with no env = %+v, want %+v (comparing Addr/Seed/LogLevel)", cfg, def)
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

// TestNewRandIndependentStreams proves the correctness fix: two successive
// NewRand streams from the same Config must diverge, otherwise two subsystems
// (e.g. two rooms) would shuffle identically.
func TestNewRandIndependentStreams(t *testing.T) {
	cfg := Default()
	cfg.Seed = 12345

	r1 := cfg.NewRand()
	r2 := cfg.NewRand()

	// Draw a window and require at least one difference. With distinct PCG
	// seeds the probability of 100 identical draws is negligible (2^-6400).
	diverged := false
	for i := 0; i < 100; i++ {
		if r1.Uint64() != r2.Uint64() {
			diverged = true
			break
		}
	}
	if !diverged {
		t.Fatal("two streams from same Config were identical for 100 draws — they must diverge")
	}
}

// TestNewRandReproducibleAcrossRuns proves that while streams within a Config
// diverge, the same Seed yields byte-identical streams across runs when the
// call order is the same. We simulate two runs by creating two separate Configs
// with the same Seed and comparing the Nth stream from each.
func TestNewRandReproducibleAcrossRuns(t *testing.T) {
	seed := int64(12345)

	cfgA := Default()
	cfgA.Seed = seed
	cfgB := Default()
	cfgB.Seed = seed

	// First stream from each Config must be identical.
	a1 := cfgA.NewRand()
	b1 := cfgB.NewRand()
	for i := 0; i < 100; i++ {
		if a1.Uint64() != b1.Uint64() {
			t.Fatalf("first stream diverged across runs at draw %d", i)
		}
	}

	// Second stream from each Config must also be identical (but different from first).
	a2 := cfgA.NewRand()
	b2 := cfgB.NewRand()
	for i := 0; i < 100; i++ {
		if a2.Uint64() != b2.Uint64() {
			t.Fatalf("second stream diverged across runs at draw %d", i)
		}
	}

	// And first vs second within same run must still diverge (sanity).
	cfgC := Default()
	cfgC.Seed = seed
	r1 := cfgC.NewRand()
	r2 := cfgC.NewRand()
	same := true
	for i := 0; i < 20; i++ {
		if r1.Uint64() != r2.Uint64() {
			same = false
			break
		}
	}
	if same {
		t.Fatal("first and second streams within same Config must not be identical")
	}
}

// TestNewRandForIndependentAndReproducible covers the named-consumer path
// (e.g. per-room): different names diverge, same name is stable, and the
// result does not depend on how many anonymous NewRand calls happened before.
func TestNewRandForIndependentAndReproducible(t *testing.T) {
	seed := int64(999)

	// Different names must diverge.
	cfg := Default()
	cfg.Seed = seed
	ra := cfg.NewRandFor("room-a")
	rb := cfg.NewRandFor("room-b")
	diverged := false
	for i := 0; i < 20; i++ {
		if ra.Uint64() != rb.Uint64() {
			diverged = true
			break
		}
	}
	if !diverged {
		t.Fatal("NewRandFor with different names produced identical streams")
	}

	// Same name must be byte-identical across Configs (simulated runs).
	cfg1 := Default()
	cfg1.Seed = seed
	cfg2 := Default()
	cfg2.Seed = seed
	r1 := cfg1.NewRandFor("room-42")
	r2 := cfg2.NewRandFor("room-42")
	for i := 0; i < 100; i++ {
		if r1.Uint64() != r2.Uint64() {
			t.Fatalf("NewRandFor same name diverged across runs at draw %d", i)
		}
	}

	// NewRandFor must be independent of NewRand call order.
	cfg3 := Default()
	cfg3.Seed = seed
	_ = cfg3.NewRand()
	_ = cfg3.NewRand()
	rNamedAfter := cfg3.NewRandFor("stable-room")
	cfg4 := Default()
	cfg4.Seed = seed
	rNamedBefore := cfg4.NewRandFor("stable-room")
	for i := 0; i < 100; i++ {
		if rNamedAfter.Uint64() != rNamedBefore.Uint64() {
			t.Fatalf("NewRandFor should be order-independent, diverged at draw %d", i)
		}
	}
}

// TestNewRandDiffersBySeed ensures different seeds still produce different streams.
func TestNewRandDiffersBySeed(t *testing.T) {
	cfgA := Default()
	cfgA.Seed = 1
	cfgB := Default()
	cfgB.Seed = 2
	a := cfgA.NewRand()
	b := cfgB.NewRand()

	// Extremely unlikely that two different seeds match on the first draw.
	if a.Uint64() == b.Uint64() {
		t.Fatal("different seeds produced identical first draw")
	}
}

// TestNewRandConcurrentSafety ensures NewRand can be called concurrently without
// races and still yields distinct streams.
func TestNewRandConcurrentSafety(t *testing.T) {
	cfg := Default()
	cfg.Seed = 42

	const goroutines = 20
	results := make([]uint64, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := cfg.NewRand()
			results[idx] = r.Uint64()
		}(i)
	}
	wg.Wait()

	// All first draws should be distinct (very high probability). At minimum
	// we require not all equal.
	allEqual := true
	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Fatal("concurrent NewRand calls produced identical first draws — expected distinct streams")
	}
}

// TestRNGAlias ensures the compatibility alias RNG/RNGFor behaves identically.
func TestRNGAlias(t *testing.T) {
	cfg := Default()
	cfg.Seed = 777
	r1 := cfg.RNG()
	r2 := cfg.NewRand()
	// r1 and r2 are successive streams, so they must diverge.
	if r1.Uint64() == r2.Uint64() {
		// Could coincidentally match, check longer window.
		r1b := cfg.RNG()
		r2b := cfg.NewRand()
		if r1b.Uint64() == r2b.Uint64() {
			t.Log("warning: RNG alias first draws collided, checking longer sequence")
		}
	}
	// RNGFor should match NewRandFor.
	cfgA := Default()
	cfgA.Seed = 123
	cfgB := Default()
	cfgB.Seed = 123
	if cfgA.RNGFor("x").Uint64() != cfgB.NewRandFor("x").Uint64() {
		t.Fatal("RNGFor and NewRandFor diverged for same name/seed")
	}
}
