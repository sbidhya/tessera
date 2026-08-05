package engine

import "testing"

func chipsEmpty() [BoardSize][BoardSize]int {
	var c [BoardSize][BoardSize]int
	for r := 0; r < BoardSize; r++ {
		for col := 0; col < BoardSize; col++ {
			c[r][col] = chipEmpty
		}
	}
	return c
}

func place(chips *[BoardSize][BoardSize]int, player int, positions []Position) {
	for _, p := range positions {
		chips[p.Row][p.Col] = player
	}
}

func TestFindSequencesHorizontal(t *testing.T) {
	chips := chipsEmpty()
	// Horizontal line at row 5, cols 2-6
	for c := 2; c <= 6; c++ {
		chips[5][c] = 0
	}
	seqs := FindSequences(chips, 0)
	if len(seqs) != 1 {
		t.Fatalf("horizontal seq count = %d want 1 got %v", len(seqs), seqs)
	}
	if len(seqs[0].Cells) != 5 {
		t.Errorf("seq len = %d want 5", len(seqs[0].Cells))
	}
	// Other player should have 0
	if got := FindSequences(chips, 1); len(got) != 0 {
		t.Errorf("player 1 seqs = %d want 0", len(got))
	}
}

func TestFindSequencesVertical(t *testing.T) {
	chips := chipsEmpty()
	for r := 1; r <= 5; r++ {
		chips[r][3] = 1
	}
	seqs := FindSequences(chips, 1)
	if len(seqs) != 1 {
		t.Fatalf("vertical seq count = %d want 1", len(seqs))
	}
}

func TestFindSequencesDiagonalDownRight(t *testing.T) {
	chips := chipsEmpty()
	for k := 0; k < 5; k++ {
		chips[2+k][2+k] = 0
	}
	if got := FindSequences(chips, 0); len(got) != 1 {
		t.Fatalf("diag DR count = %d want 1", len(got))
	}
}

func TestFindSequencesDiagonalDownLeft(t *testing.T) {
	chips := chipsEmpty()
	for k := 0; k < 5; k++ {
		chips[2+k][7-k] = 0
	}
	if got := FindSequences(chips, 0); len(got) != 1 {
		t.Fatalf("diag DL count = %d want 1", len(got))
	}
}

func TestFindSequencesCornerWild(t *testing.T) {
	tests := []struct {
		name      string
		positions []Position // player 0 chips, corners are implicit wild
		want      int
	}{
		{
			name: "top row with left corner",
			// Corner (0,0) + 4 chips at (0,1)-(0,4) => 5 in row
			positions: []Position{{0, 1}, {0, 2}, {0, 3}, {0, 4}},
			want:      1,
		},
		{
			name: "top row with right corner",
			positions: []Position{{0, 5}, {0, 6}, {0, 7}, {0, 8}},
			want:      1, // (0,5)-(0,8) + (0,9) corner
		},
		{
			name: "left column with top corner",
			positions: []Position{{1, 0}, {2, 0}, {3, 0}, {4, 0}},
			want:      1,
		},
		{
			name: "diagonal from corner",
			// Corner (0,0) wild plus diagonal 1,1 2,2 3,3 4,4
			positions: []Position{{1, 1}, {2, 2}, {3, 3}, {4, 4}},
			want:      1,
		},
		{
			name: "corner at end of vertical line bottom",
			positions: []Position{{5, 9}, {6, 9}, {7, 9}, {8, 9}},
			want:      1,
		},
		{
			name: "four chips without corner not a sequence",
			positions: []Position{{0, 1}, {0, 2}, {0, 3}, {0, 5}},
			want:      0,
		},
		{
			name: "five chips including corner counted even if only 4 non-corner",
			positions: []Position{{1, 1}, {2, 2}, {3, 3}, {4, 4}},
			want:      1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chips := chipsEmpty()
			place(&chips, 0, tc.positions)
			seqs := FindSequences(chips, 0)
			if len(seqs) != tc.want {
				t.Fatalf("seq count = %d want %d, seqs=%v", len(seqs), tc.want, seqs)
			}
			if tc.want == 1 {
				// Verify the sequence indeed includes a corner
				hasCorner := false
				for _, c := range seqs[0].Cells {
					if IsCorner(c.Row, c.Col) {
						hasCorner = true
						break
					}
				}
				if !hasCorner {
					t.Error("expected sequence to include a corner wild")
				}
			}
		})
	}
}

func TestFindSequencesNoFalsePositive(t *testing.T) {
	chips := chipsEmpty()
	// 4 in a row should not count
	for c := 0; c < 4; c++ {
		chips[0][c] = 0
	}
	// Even though (0,0) is corner, need 5 total. 4 chips + maybe gap
	if got := FindSequences(chips, 0); len(got) != 0 {
		t.Fatalf("4 in row counted as sequence: %v", got)
	}
	// Gap in middle
	chips = chipsEmpty()
	chips[5][2] = 0
	chips[5][3] = 0
	chips[5][5] = 0
	chips[5][6] = 0
	chips[5][4] = 1 // opponent chip breaks line
	if got := FindSequences(chips, 0); len(got) != 0 {
		t.Fatalf("gapped line counted: %v", got)
	}
}

func TestFindSequencesMultipleAndPacking(t *testing.T) {
	// Two separate horizontal sequences far apart should count as 2
	chips := chipsEmpty()
	for c := 1; c <= 5; c++ {
		chips[2][c] = 0
	}
	for c := 1; c <= 5; c++ {
		chips[7][c] = 0
	}
	seqs := FindSequences(chips, 0)
	if len(seqs) != 2 {
		t.Fatalf("two separate seqs = %d want 2", len(seqs))
	}
}

func TestFindSequencesOverlappingPacking(t *testing.T) {
	// 6 in a row horizontally at row 5 col 2-7 (6 chips). Naive window count would be 2
	// (2-6 and 3-7) but packed greedy should count only 1 because they overlap.
	chips := chipsEmpty()
	for c := 2; c <= 7; c++ {
		chips[5][c] = 0
	}
	seqs := FindSequences(chips, 0)
	if len(seqs) != 1 {
		t.Fatalf("overlapping 6-run packed = %d want 1, got %v", len(seqs), seqs)
	}

	// 10 in a row should be 2 non-overlapping sequences (0-4 and 5-9)
	chips = chipsEmpty()
	for c := 0; c <= 9; c++ {
		// avoid corners? row 5 has no corners
		chips[5][c] = 0
	}
	// But col 0 and 9 on row 5 are not corners (corners are 0,0 etc). Row 5 col 0 is not corner.
	seqs = FindSequences(chips, 0)
	if len(seqs) != 2 {
		t.Fatalf("10-run packed = %d want 2, got %v", len(seqs), seqs)
	}
}

func TestFindSequencesCornerOverlapAllowed(t *testing.T) {
	// Two sequences that share only a corner wild should both count (corners not marked used)
	chips := chipsEmpty()
	// Top row sequence using (0,0) corner: chips at (0,1)-(0,4)
	for c := 1; c <= 4; c++ {
		chips[0][c] = 0
	}
	// Left column sequence using same (0,0) corner: chips at (1,0)-(4,0)
	for r := 1; r <= 4; r++ {
		chips[r][0] = 0
	}
	seqs := FindSequences(chips, 0)
	if len(seqs) != 2 {
		t.Fatalf("corner-shared seqs = %d want 2, got %v", len(seqs), seqs)
	}
}

func TestLockedGrid(t *testing.T) {
	chips := chipsEmpty()
	for c := 2; c <= 6; c++ {
		chips[3][c] = 1
	}
	locked := LockedGrid(chips, 2)
	for c := 2; c <= 6; c++ {
		if !locked[3][c] {
			t.Errorf("cell 3,%d should be locked", c)
		}
	}
	if locked[3][1] {
		t.Error("cell outside sequence should not be locked")
	}
	// Corners never locked even if part of sequence
	chips = chipsEmpty()
	for c := 1; c <= 4; c++ {
		chips[0][c] = 0
	}
	locked = LockedGrid(chips, 2)
	if locked[0][0] {
		t.Error("corner should never be locked")
	}
}

func TestAllSequences(t *testing.T) {
	chips := chipsEmpty()
	for c := 0; c < 5; c++ {
		chips[1][c] = 0
	}
	for c := 0; c < 5; c++ {
		chips[8][c] = 1
	}
	all := AllSequences(chips, 2)
	if len(all[0]) != 1 || len(all[1]) != 1 {
		t.Fatalf("AllSequences counts = %v want [1 1]", []int{len(all[0]), len(all[1])})
	}
}

func TestSequenceDirectionsTable(t *testing.T) {
	// Table-driven check for all 4 directions plus corner case
	cases := []struct {
		name   string
		mk     func() [BoardSize][BoardSize]int
		player int
		want   int
	}{
		{
			name: "horizontal",
			mk: func() [BoardSize][BoardSize]int {
				c := chipsEmpty()
				for c2 := 3; c2 < 8; c2++ {
					c[5][c2] = 0
				}
				return c
			},
			player: 0,
			want:   1,
		},
		{
			name: "vertical",
			mk: func() [BoardSize][BoardSize]int {
				c := chipsEmpty()
				for r := 3; r < 8; r++ {
					c[r][5] = 0
				}
				return c
			},
			player: 0,
			want:   1,
		},
		{
			name: "diag DR",
			mk: func() [BoardSize][BoardSize]int {
				c := chipsEmpty()
				for k := 0; k < 5; k++ {
					c[3+k][3+k] = 0
				}
				return c
			},
			player: 0,
			want:   1,
		},
		{
			name: "diag DL",
			mk: func() [BoardSize][BoardSize]int {
				c := chipsEmpty()
				for k := 0; k < 5; k++ {
					c[3+k][7-k] = 0
				}
				return c
			},
			player: 0,
			want:   1,
		},
		{
			name: "with corner horizontal top",
			mk: func() [BoardSize][BoardSize]int {
				c := chipsEmpty()
				for col := 1; col <= 4; col++ {
					c[0][col] = 1
				}
				return c
			},
			player: 1,
			want:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chips := tc.mk()
			if got := len(FindSequences(chips, tc.player)); got != tc.want {
				t.Fatalf("seq count = %d want %d", got, tc.want)
			}
		})
	}
}
