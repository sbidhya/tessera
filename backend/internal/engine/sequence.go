package engine

// directions are the four line orientations a sequence can run along: horizontal,
// vertical, and the two diagonals. Only one of each ± pair is needed because a
// window is scanned in both directions from the placed cell.
var directions = [4]Cell{
	{0, 1},  // horizontal
	{1, 0},  // vertical
	{1, 1},  // diagonal ↘
	{1, -1}, // diagonal ↙
}

// countsFor reports whether cell x counts toward a sequence for player p: either
// a wild corner (counts for everyone) or a cell holding p's chip.
func (gs *GameState) countsFor(x Cell, p PlayerID) bool {
	if !x.InBounds() {
		return false
	}
	if gs.Board.IsCorner(x) {
		return true
	}
	ch, ok := gs.Chips[x]
	return ok && ch.Owner == p
}

// lockedCells returns the set of non-corner cells already locked into a completed
// sequence owned by p. The overlap rule limits a new sequence to sharing at most
// one such cell (corners are wild and shared freely, so they are excluded).
func (gs *GameState) lockedCells(p PlayerID) map[Cell]bool {
	locked := make(map[Cell]bool)
	for _, seq := range gs.Sequences {
		if seq.Owner != p {
			continue
		}
		for _, c := range seq.Cells {
			if !gs.Board.IsCorner(c) {
				locked[c] = true
			}
		}
	}
	return locked
}

// detectSequencesThrough finds new completed sequences for player p that pass
// through the just-changed cell, records them, marks their chips as locked, and
// returns how many new sequences were found.
//
// A candidate is a run of 5 consecutive cells along one of the four directions,
// all of which count for p (chip or corner), and which includes the placed cell.
// A candidate becomes a NEW sequence only if it shares at most one already-locked
// non-corner cell with p's existing sequences (the standard "reuse one chip"
// rule) and is not a duplicate of a sequence already recorded this call.
func (gs *GameState) detectSequencesThrough(placed Cell, p PlayerID) int {
	locked := gs.lockedCells(p)
	found := 0

	// seen dedupes identical windows discovered from different offsets/directions
	// within this single detection pass.
	seen := make(map[[5]Cell]bool)

	for _, d := range directions {
		// Windows of 5 that include `placed`: start at placed shifted back by i
		// steps, for i in 0..4.
		for i := 0; i < 5; i++ {
			start := Cell{Row: placed.Row - i*d.Row, Col: placed.Col - i*d.Col}
			var window [5]Cell
			ok := true
			for k := 0; k < 5; k++ {
				c := Cell{Row: start.Row + k*d.Row, Col: start.Col + k*d.Col}
				if !gs.countsFor(c, p) {
					ok = false
					break
				}
				window[k] = c
			}
			if !ok || seen[window] {
				continue
			}
			seen[window] = true

			// Overlap rule: at most one non-corner cell may already be locked.
			shared := 0
			for _, c := range window {
				if locked[c] {
					shared++
				}
			}
			if shared > 1 {
				continue
			}

			// Accept: record the sequence, lock its chips, and fold its cells
			// into `locked` so a second window this pass respects the rule too.
			gs.Sequences = append(gs.Sequences, Sequence{Owner: p, Cells: window})
			for _, c := range window {
				if gs.Board.IsCorner(c) {
					continue
				}
				ch := gs.Chips[c]
				ch.InSequence = true
				gs.Chips[c] = ch
				locked[c] = true
			}
			found++
		}
	}
	return found
}
