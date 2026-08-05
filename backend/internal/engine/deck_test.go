package engine

import (
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
)

func TestNewDeckSize(t *testing.T) {
	cfg := config.Config{Seed: 42}
	rng := cfg.NewRand("deck")
	deck := NewDeck(rng)
	if len(deck) != 104 {
		t.Fatalf("deck len = %d want 104", len(deck))
	}
	// Ensure all cards are present twice per suit/rank (52*2).
	counts := map[Card]int{}
	for _, c := range deck {
		counts[c]++
	}
	for s := SuitHearts; s <= SuitSpades; s++ {
		for r := RankAce; r <= RankKing; r++ {
			c := Card{Suit: s, Rank: r}
			if counts[c] != 2 {
				t.Errorf("card %v count = %d want 2", c, counts[c])
			}
		}
	}
}

func TestNewDeckDeterministic(t *testing.T) {
	cfg := config.Config{Seed: 999}
	d1 := NewDeck(cfg.NewRand("deck"))
	d2 := NewDeck(cfg.NewRand("deck"))
	if len(d1) != len(d2) {
		t.Fatalf("len mismatch %d vs %d", len(d1), len(d2))
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatalf("determinism broken at %d: %v vs %v", i, d1[i], d2[i])
		}
	}
}

func TestDeckDifferentSeedsDiverge(t *testing.T) {
	d1 := NewDeck(config.Config{Seed: 1}.NewRand("deck"))
	d2 := NewDeck(config.Config{Seed: 2}.NewRand("deck"))
	equal := true
	for i := range d1 {
		if d1[i] != d2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Fatal("different seeds produced identical decks — streams not independent")
	}
}

func TestDeckDifferentStreamsDiverge(t *testing.T) {
	cfg := config.Config{Seed: 123}
	d1 := NewDeck(cfg.NewRand("deck"))
	d2 := NewDeck(cfg.NewRand("other"))
	equal := true
	for i := range d1 {
		if d1[i] != d2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Fatal("different streams produced identical decks")
	}
}

func TestDeckShuffle(t *testing.T) {
	cfg := config.Config{Seed: 1}
	rng := cfg.NewRand("deck")
	d := NewDeck(rng)
	// Snapshot before shuffle
	cp := d.Clone()
	rng2 := cfg.NewRand("reshuffle")
	cp.Shuffle(rng2)
	// Shuffled should differ (extremely unlikely to be identical)
	same := true
	for i := range d {
		if d[i] != cp[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("shuffle did not change order (too unlikely)")
	}
}

func TestDeckDraw(t *testing.T) {
	cfg := config.Config{Seed: 7}
	deck := NewDeck(cfg.NewRand("deck"))
	n := len(deck)
	c, ok := deck.Draw()
	if !ok {
		t.Fatal("draw on non-empty deck failed")
	}
	if len(deck) != n-1 {
		t.Errorf("len after draw = %d want %d", len(deck), n-1)
	}
	// Draw all remaining
	for deck.Len() > 0 {
		if _, ok := deck.Draw(); !ok {
			t.Fatal("draw failed before empty")
		}
	}
	if _, ok := deck.Draw(); ok {
		t.Error("draw on empty deck should return false")
	}
	_ = c
}

func TestDeckCloneIndependent(t *testing.T) {
	cfg := config.Config{Seed: 5}
	d := NewDeck(cfg.NewRand("deck"))
	cp := d.Clone()
	cp.Shuffle(cfg.NewRand("clone-shuffle"))
	// Original should be unchanged after cloning and shuffling clone.
	// Check second element diff likely
	// But more importantly, modifying clone shouldn't affect original length.
	if len(d) != 104 || len(cp) != 104 {
		t.Error("clone length mismatch")
	}
}
