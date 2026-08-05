package engine

import (
	"math/rand/v2"
	"testing"
)

// testRand returns a deterministic RNG for tests. A fixed seed keeps every test
// reproducible, matching the project's seeded-RNG principle.
func testRand() *rand.Rand {
	return rand.New(rand.NewPCG(0xC0FFEE, 0xBADC0DE))
}

// rngFrom builds a deterministic RNG from a single seed value, for tests that
// vary the seed to cover many deals.
func rngFrom(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed*0x9e3779b97f4a7c15+1))
}

func TestCardString(t *testing.T) {
	cases := []struct {
		card Card
		want string
	}{
		{Card{Ace, Spades}, "A♠"},
		{Card{Ten, Diamonds}, "10♦"},
		{Card{King, Hearts}, "K♥"},
		{Card{Jack, Clubs}, "J♣"},
		{Card{Two, Clubs}, "2♣"},
	}
	for _, tc := range cases {
		if got := tc.card.String(); got != tc.want {
			t.Errorf("Card{%d,%d}.String() = %q, want %q", tc.card.Rank, tc.card.Suit, got, tc.want)
		}
	}
}

func TestJackClassification(t *testing.T) {
	cases := []struct {
		card             Card
		isJack, two, one bool
	}{
		{Card{Jack, Diamonds}, true, true, false},
		{Card{Jack, Clubs}, true, true, false},
		{Card{Jack, Hearts}, true, false, true},
		{Card{Jack, Spades}, true, false, true},
		{Card{Ace, Spades}, false, false, false},
		{Card{Ten, Diamonds}, false, false, false},
	}
	for _, tc := range cases {
		if got := tc.card.IsJack(); got != tc.isJack {
			t.Errorf("%s IsJack = %v, want %v", tc.card, got, tc.isJack)
		}
		if got := tc.card.IsTwoEyedJack(); got != tc.two {
			t.Errorf("%s IsTwoEyedJack = %v, want %v", tc.card, got, tc.two)
		}
		if got := tc.card.IsOneEyedJack(); got != tc.one {
			t.Errorf("%s IsOneEyedJack = %v, want %v", tc.card, got, tc.one)
		}
	}
}

func TestNewDeckComposition(t *testing.T) {
	deck := NewDeck()
	if len(deck) != 104 {
		t.Fatalf("deck size = %d, want 104 (two full decks)", len(deck))
	}
	counts := make(map[Card]int)
	for _, c := range deck {
		counts[c]++
	}
	if len(counts) != 52 {
		t.Fatalf("distinct cards = %d, want 52", len(counts))
	}
	for c, n := range counts {
		if n != 2 {
			t.Errorf("card %s appears %d times, want 2", c, n)
		}
	}
}

func TestShuffleDeterministicAndPermutation(t *testing.T) {
	d1 := NewDeck()
	d2 := NewDeck()
	Shuffle(rand.New(rand.NewPCG(1, 2)), d1)
	Shuffle(rand.New(rand.NewPCG(1, 2)), d2)

	// Same RNG seed → identical shuffle.
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatalf("shuffle not deterministic at %d: %s != %s", i, d1[i], d2[i])
		}
	}

	// Shuffle preserves multiset composition.
	orig := NewDeck()
	countMultiset := func(cards []Card) map[Card]int {
		m := make(map[Card]int)
		for _, c := range cards {
			m[c]++
		}
		return m
	}
	o, s := countMultiset(orig), countMultiset(d1)
	for c, n := range o {
		if s[c] != n {
			t.Errorf("card %s count changed by shuffle: %d != %d", c, s[c], n)
		}
	}

	// A different seed should produce a different ordering (astronomically likely).
	d3 := NewDeck()
	Shuffle(rand.New(rand.NewPCG(9, 9)), d3)
	same := true
	for i := range d1 {
		if d1[i] != d3[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different seeds produced identical shuffle order")
	}
}
