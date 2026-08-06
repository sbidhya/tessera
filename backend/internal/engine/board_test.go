package engine

import "testing"

// TestBoardInvariants is the core board correctness check: the layout must have
// exactly the four wild corners, and every non-jack card must appear on exactly
// two cells — no more, no less. This is what makes generating the board (rather
// than hand-transcribing it) safe.
func TestBoardInvariants(t *testing.T) {
	b := NewBoard(testRand())

	// Corners are wild and carry no card; everything else carries a card.
	cornerCount := 0
	cardCells := 0
	for row := 0; row < BoardSize; row++ {
		for col := 0; col < BoardSize; col++ {
			cell := Cell{row, col}
			if b.IsCorner(cell) {
				cornerCount++
				if _, ok := b.CardAt(cell); ok {
					t.Errorf("corner %s unexpectedly has a card", cell)
				}
				continue
			}
			card, ok := b.CardAt(cell)
			if !ok {
				t.Errorf("non-corner %s has no card", cell)
				continue
			}
			if card.IsJack() {
				t.Errorf("jack %s must not appear on the board at %s", card, cell)
			}
			cardCells++
		}
	}
	if cornerCount != 4 {
		t.Errorf("corner count = %d, want 4", cornerCount)
	}
	if cardCells != 96 {
		t.Errorf("card-bearing cells = %d, want 96", cardCells)
	}

	// Every non-jack card appears exactly twice, and CellsFor agrees with CardAt.
	for _, s := range allSuits {
		for _, r := range allRanks {
			if r == Jack {
				continue
			}
			card := Card{Rank: r, Suit: s}
			cells := b.CellsFor(card)
			if len(cells) != 2 {
				t.Errorf("card %s appears on %d cells, want 2", card, len(cells))
			}
			for _, c := range cells {
				if got, _ := b.CardAt(c); got != card {
					t.Errorf("CellsFor(%s) points at %s which holds %s", card, c, got)
				}
			}
		}
	}
}

func TestBoardCornersAreTheFour(t *testing.T) {
	b := NewBoard(testRand())
	want := []Cell{{0, 0}, {0, 9}, {9, 0}, {9, 9}}
	for _, c := range want {
		if !b.IsCorner(c) {
			t.Errorf("%s should be a corner", c)
		}
	}
	if b.IsCorner(Cell{0, 1}) || b.IsCorner(Cell{5, 5}) {
		t.Error("non-corner reported as corner")
	}
}

func TestBoardDeterministic(t *testing.T) {
	b1 := NewBoard(testRand())
	b2 := NewBoard(testRand())
	for row := 0; row < BoardSize; row++ {
		for col := 0; col < BoardSize; col++ {
			c := Cell{row, col}
			a, _ := b1.CardAt(c)
			b, _ := b2.CardAt(c)
			if a != b {
				t.Fatalf("board not deterministic at %s: %s != %s", c, a, b)
			}
		}
	}
}

func TestCardAtOutOfBounds(t *testing.T) {
	b := NewBoard(testRand())
	if _, ok := b.CardAt(Cell{-1, 0}); ok {
		t.Error("CardAt out of bounds should return ok=false")
	}
	if _, ok := b.CardAt(Cell{10, 10}); ok {
		t.Error("CardAt out of bounds should return ok=false")
	}
	if b.IsCorner(Cell{-1, -1}) {
		t.Error("out-of-bounds cell should not be a corner")
	}
	if len(b.CellsFor(Card{Jack, Spades})) != 0 {
		t.Error("jacks should map to no cells")
	}
}
