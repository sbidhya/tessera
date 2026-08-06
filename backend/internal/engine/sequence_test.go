package engine

import "testing"

// detectState builds a bare GameState with a real board and no chips, for
// exercising sequence detection directly.
func detectState() *GameState {
	return &GameState{
		Board:          NewBoard(testRand()),
		Chips:          make(map[Cell]Chip),
		SequencesWon:   make(map[PlayerID]int),
		NumPlayers:     2,
		SequencesToWin: 2,
		Winner:         NoPlayer,
	}
}

// place drops chips for player p on the given cells without going through Apply,
// so detection can be tested against precise board geometry.
func (gs *GameState) place(p PlayerID, cells ...Cell) {
	for _, c := range cells {
		gs.Chips[c] = Chip{Owner: p}
	}
}

// assertLocked checks that the given cells now carry a locked (in-sequence) chip.
func assertLocked(t *testing.T, gs *GameState, cells ...Cell) {
	t.Helper()
	for _, c := range cells {
		ch, ok := gs.Chips[c]
		if !ok {
			t.Errorf("cell %s has no chip", c)
			continue
		}
		if !ch.InSequence {
			t.Errorf("cell %s chip not marked InSequence", c)
		}
	}
}

func TestDetectDirections(t *testing.T) {
	const p PlayerID = 0
	cases := []struct {
		name  string
		cells []Cell // five in a line; last is the "placed" trigger
	}{
		{"horizontal", []Cell{{5, 1}, {5, 2}, {5, 3}, {5, 4}, {5, 5}}},
		{"vertical", []Cell{{2, 5}, {3, 5}, {4, 5}, {5, 5}, {6, 5}}},
		{"diagonal-down-right", []Cell{{2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6}}},
		{"diagonal-down-left", []Cell{{2, 6}, {3, 5}, {4, 4}, {5, 3}, {6, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := detectState()
			gs.place(p, tc.cells...)
			placed := tc.cells[len(tc.cells)-1]
			if got := gs.detectSequencesThrough(placed, p); got != 1 {
				t.Fatalf("detected %d sequences, want 1", got)
			}
			assertLocked(t, gs, tc.cells...)
		})
	}
}

// TestDetectWithCorners verifies each wild corner counts toward a sequence for
// any player, in the line that runs through it.
func TestDetectWithCorners(t *testing.T) {
	const p PlayerID = 1
	cases := []struct {
		name    string
		corner  Cell
		chips   []Cell // four chips; corner is the wild fifth
		trigger Cell
	}{
		{"top-left horizontal", Cell{0, 0}, []Cell{{0, 1}, {0, 2}, {0, 3}, {0, 4}}, Cell{0, 4}},
		{"top-left vertical", Cell{0, 0}, []Cell{{1, 0}, {2, 0}, {3, 0}, {4, 0}}, Cell{4, 0}},
		{"top-left diagonal", Cell{0, 0}, []Cell{{1, 1}, {2, 2}, {3, 3}, {4, 4}}, Cell{4, 4}},
		{"bottom-right diagonal", Cell{9, 9}, []Cell{{5, 5}, {6, 6}, {7, 7}, {8, 8}}, Cell{8, 8}},
		{"top-right anti-diagonal", Cell{0, 9}, []Cell{{1, 8}, {2, 7}, {3, 6}, {4, 5}}, Cell{4, 5}},
		{"bottom-left vertical", Cell{9, 0}, []Cell{{5, 0}, {6, 0}, {7, 0}, {8, 0}}, Cell{8, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := detectState()
			gs.place(p, tc.chips...)
			if got := gs.detectSequencesThrough(tc.trigger, p); got != 1 {
				t.Fatalf("detected %d sequences, want 1", got)
			}
			assertLocked(t, gs, tc.chips...)
			// The corner itself must never hold a chip.
			if _, ok := gs.Chips[tc.corner]; ok {
				t.Errorf("corner %s should not carry a chip", tc.corner)
			}
		})
	}
}

func TestDetectFourIsNotEnough(t *testing.T) {
	gs := detectState()
	cells := []Cell{{5, 1}, {5, 2}, {5, 3}, {5, 4}}
	gs.place(0, cells...)
	if got := gs.detectSequencesThrough(Cell{5, 4}, 0); got != 0 {
		t.Fatalf("four in a row detected %d sequences, want 0", got)
	}
}

func TestDetectIgnoresOpponentChips(t *testing.T) {
	gs := detectState()
	// A run of five, but one cell belongs to the opponent — no sequence for p=0.
	gs.place(0, Cell{5, 1}, Cell{5, 2}, Cell{5, 4}, Cell{5, 5})
	gs.place(1, Cell{5, 3})
	if got := gs.detectSequencesThrough(Cell{5, 5}, 0); got != 0 {
		t.Fatalf("mixed-owner line detected %d sequences for p0, want 0", got)
	}
}

// TestDetectSharesAtMostOneCell verifies the overlap rule: a new sequence may
// reuse at most one cell from an existing sequence.
func TestDetectSharesAtMostOneCell(t *testing.T) {
	const p PlayerID = 0

	t.Run("reusing one cell is allowed", func(t *testing.T) {
		gs := detectState()
		// First sequence: cols 0..4 on row 5.
		gs.place(p, Cell{5, 0}, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
		if got := gs.detectSequencesThrough(Cell{5, 4}, p); got != 1 {
			t.Fatalf("first sequence: got %d, want 1", got)
		}
		// Extend right: cols 5..8. The window cols 4..8 shares exactly one locked
		// cell (col 4), so it counts as a new sequence.
		gs.place(p, Cell{5, 5}, Cell{5, 6}, Cell{5, 7}, Cell{5, 8})
		if got := gs.detectSequencesThrough(Cell{5, 8}, p); got != 1 {
			t.Fatalf("second sequence sharing one cell: got %d, want 1", got)
		}
	})

	t.Run("reusing two cells is rejected", func(t *testing.T) {
		gs := detectState()
		// First sequence: cols 0..4 on row 5.
		gs.place(p, Cell{5, 0}, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
		gs.detectSequencesThrough(Cell{5, 4}, p)
		// Add col 5. The only new full window is cols 1..5, which shares four
		// locked cells (1..4) — far more than one — so it must not count.
		gs.place(p, Cell{5, 5})
		if got := gs.detectSequencesThrough(Cell{5, 5}, p); got != 0 {
			t.Fatalf("window sharing multiple locked cells: got %d, want 0", got)
		}
	})
}

// TestDetectTwoSequencesAtOnce verifies a single placement at the intersection of
// a ready horizontal and a ready vertical line completes both.
func TestDetectTwoSequencesAtOnce(t *testing.T) {
	const p PlayerID = 0
	gs := detectState()
	// Horizontal on row 5 missing the center (5,5).
	gs.place(p, Cell{5, 3}, Cell{5, 4}, Cell{5, 6}, Cell{5, 7})
	// Vertical on col 5 missing the center (5,5).
	gs.place(p, Cell{3, 5}, Cell{4, 5}, Cell{6, 5}, Cell{7, 5})
	// Place the shared center.
	gs.place(p, Cell{5, 5})
	if got := gs.detectSequencesThrough(Cell{5, 5}, p); got != 2 {
		t.Fatalf("cross placement detected %d sequences, want 2", got)
	}
}

// TestDetectNoDoubleCount ensures re-running detection through a cell in an
// already-recorded sequence finds nothing new.
func TestDetectNoDoubleCount(t *testing.T) {
	const p PlayerID = 0
	gs := detectState()
	gs.place(p, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4}, Cell{5, 5})
	if got := gs.detectSequencesThrough(Cell{5, 5}, p); got != 1 {
		t.Fatalf("first detect: got %d, want 1", got)
	}
	if got := gs.detectSequencesThrough(Cell{5, 3}, p); got != 0 {
		t.Fatalf("re-detect through same line: got %d, want 0", got)
	}
}
