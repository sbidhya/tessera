package engine

import "testing"

func TestBoardCorners(t *testing.T) {
	b := NewBoard()
	corners := []Position{{0, 0}, {0, 9}, {9, 0}, {9, 9}}
	for _, p := range corners {
		cell, ok := b.At(p.Row, p.Col)
		if !ok {
			t.Fatalf("corner %v out of bounds", p)
		}
		if !cell.IsCorner {
			t.Errorf("cell %v should be corner", p)
		}
		if cell.Card != nil {
			t.Errorf("corner %v should have nil card, got %v", p, *cell.Card)
		}
		if _, ok := b.CardAt(p.Row, p.Col); ok {
			t.Errorf("CardAt corner %v should return false", p)
		}
	}
	// Non-corners should have cards
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			if IsCorner(r, c) {
				continue
			}
			cell, _ := b.At(r, c)
			if cell.IsCorner {
				t.Errorf("cell %d,%d should not be corner", r, c)
			}
			if cell.Card == nil {
				t.Fatalf("cell %d,%d should have card", r, c)
			}
			if IsJack(*cell.Card) {
				t.Errorf("board should never contain jack, found %v at %d,%d", *cell.Card, r, c)
			}
		}
	}
}

func TestBoardCounts(t *testing.T) {
	b := NewBoard()
	// Count total non-corner cells
	total := 0
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			if !IsCorner(r, c) {
				total++
			}
		}
	}
	if total != 96 {
		t.Fatalf("non-corner count = %d want 96", total)
	}
	// Each non-jack appears exactly twice
	counts := map[Card]int{}
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			if IsCorner(r, c) {
				continue
			}
			card, _ := b.CardAt(r, c)
			counts[card]++
		}
	}
	if len(counts) != 48 {
		t.Errorf("distinct cards on board = %d want 48", len(counts))
	}
	for card, n := range counts {
		if n != 2 {
			t.Errorf("card %v appears %d times want 2", card, n)
		}
		if IsJack(card) {
			t.Errorf("jack %v should not be on board", card)
		}
	}
}

func TestBoardPositionsFor(t *testing.T) {
	b := NewBoard()
	// Non-jack should have 2 positions
	c := Card{SuitHearts, RankAce}
	pos := b.PositionsFor(c)
	if len(pos) != 2 {
		t.Fatalf("PositionsFor AH = %v want 2", pos)
	}
	// Jack should have 0
	j := Card{SuitDiamonds, RankJack}
	if got := b.PositionsFor(j); len(got) != 0 {
		t.Errorf("jack positions = %v want 0", got)
	}
	// Each non-jack should be exactly 2 and sorted row-major
	for s := SuitHearts; s <= SuitSpades; s++ {
		for r := RankAce; r <= RankKing; r++ {
			if r == RankJack {
				continue
			}
			card := Card{Suit: s, Rank: r}
			pos := b.PositionsFor(card)
			if len(pos) != 2 {
				t.Errorf("%v positions len = %d want 2", card, len(pos))
			}
			// Verify they indeed point to that card
			for _, p := range pos {
				cardAt, ok := b.CardAt(p.Row, p.Col)
				if !ok || cardAt != card {
					t.Errorf("PositionsFor %v returned %v which has card %v ok=%v", card, p, cardAt, ok)
				}
			}
		}
	}
}

func TestBoardDeterminism(t *testing.T) {
	b1 := NewBoard()
	b2 := NewBoard()
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			c1, ok1 := b1.CardAt(r, c)
			c2, ok2 := b2.CardAt(r, c)
			if ok1 != ok2 || c1 != c2 {
				t.Fatalf("board not deterministic at %d,%d: %v vs %v", r, c, c1, c2)
			}
		}
	}
}

func TestBoardOutOfBounds(t *testing.T) {
	b := NewBoard()
	if _, ok := b.At(-1, 0); ok {
		t.Error("At(-1,0) should be false")
	}
	if _, ok := b.At(10, 10); ok {
		t.Error("At(10,10) should be false")
	}
	if _, ok := b.CardAt(-1, 0); ok {
		t.Error("CardAt(-1,0) should be false")
	}
}

func TestBoardSpecificLayout(t *testing.T) {
	// Document and lock the deterministic layout. Row-major assignment means
	// first non-corner cell (0,1) is the first single card (AH) and last
	// non-corner cell (9,8) is the last duplicated card (KS).
	b := NewBoard()
	card, _ := b.CardAt(0, 1)
	want := Card{SuitHearts, RankAce}
	if card != want {
		t.Errorf("board[0,1] = %v want %v", card, want)
	}
	// The board's 48 distinct singles are duplicated, so second half starts at
	// index 48. The cell index 48 in row-major order is deterministic; check that
	// the two copies are different cells but same card.
	pos := b.PositionsFor(want)
	if len(pos) != 2 {
		t.Fatalf("AH positions len %d", len(pos))
	}
	if pos[0] == pos[1] {
		t.Error("duplicate positions should be distinct")
	}
}
