package engine

// Sequence is a run of 5 chips belonging to one player (corners count as
// wild for everyone). Cells are in board order along the line.
type Sequence struct {
	Player int        `json:"player"`
	Cells  []Position `json:"cells"` // always 5
}

// FindSequences returns all 5-in-a-row sequences for player on the given
// chip grid. The algorithm:
//
//  1. Enumerate every length-5 window in the four directions (horizontal,
//     vertical, diagonal down-right, diagonal down-left). A window is a
//     sequence if every non-corner cell is occupied by player (corners are
//     wild and always count).
//  2. Greedily pack them into a maximal set of non-overlapping sequences
//     (sharing no non-corner chip) in deterministic scan order
//     (top-left → bottom-right, directions H, V, DR, DL). This matches the
//     physical intuition that a line of 6 chips counts as one sequence, not
//     two overlapping ones, while keeping the count deterministic and cheap.
//
// Corners are never marked as used so two sequences that meet at a corner
// remain distinct. Locked-chip checks elsewhere rely on this packing to decide
// which chips are "part of a completed sequence" and thus un-removable by
// one-eyed jacks.
func FindSequences(chips [BoardSize][BoardSize]int, player int) []Sequence {
	type window struct {
		cells []Position
	}
	var windows []window
	dirs := [][2]int{{0, 1}, {1, 0}, {1, 1}, {1, -1}}
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			for _, d := range dirs {
				dr, dc := d[0], d[1]
				endR := r + dr*4
				endC := c + dc*4
				if endR < 0 || endR >= BoardSize || endC < 0 || endC >= BoardSize {
					continue
				}
				valid := true
				var cells []Position
				for k := 0; k < 5; k++ {
					rr := r + dr*k
					cc := c + dc*k
					if IsCorner(rr, cc) {
						// wild for everyone
					} else if chips[rr][cc] != player {
						valid = false
						break
					}
					cells = append(cells, Position{Row: rr, Col: cc})
				}
				if valid {
					windows = append(windows, window{cells: cells})
				}
			}
		}
	}

	// Greedy disjoint packing.
	used := [BoardSize][BoardSize]bool{}
	var out []Sequence
	for _, w := range windows {
		overlap := false
		for _, p := range w.cells {
			if IsCorner(p.Row, p.Col) {
				continue
			}
			if used[p.Row][p.Col] {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		for _, p := range w.cells {
			if IsCorner(p.Row, p.Col) {
				continue
			}
			used[p.Row][p.Col] = true
		}
		out = append(out, Sequence{Player: player, Cells: append([]Position(nil), w.cells...)})
	}
	return out
}

// AllSequences returns the packed sequences for every player index present
// in numPlayers (expected 2 for v1). Players without chips simply have none.
func AllSequences(chips [BoardSize][BoardSize]int, numPlayers int) [][]Sequence {
	out := make([][]Sequence, numPlayers)
	for p := 0; p < numPlayers; p++ {
		out[p] = FindSequences(chips, p)
	}
	return out
}

// LockedGrid returns a 10×10 boolean grid where true means the chip at that
// cell is part of at least one counted (packed) sequence and therefore cannot
// be removed by a one-eyed jack. Corners are always false.
func LockedGrid(chips [BoardSize][BoardSize]int, numPlayers int) [BoardSize][BoardSize]bool {
	var locked [BoardSize][BoardSize]bool
	for p := 0; p < numPlayers; p++ {
		for _, seq := range FindSequences(chips, p) {
			for _, c := range seq.Cells {
				if IsCorner(c.Row, c.Col) {
					continue
				}
				locked[c.Row][c.Col] = true
			}
		}
	}
	return locked
}
