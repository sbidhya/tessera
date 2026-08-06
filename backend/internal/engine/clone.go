package engine

// Clone returns a deep copy of the GameState. The Board is immutable and is
// shared (not copied). All mutable fields — maps, slices — are freshly
// allocated so the clone can be mutated or handed to a caller without affecting
// the authoritative copy owned by the room goroutine.
//
// The room manager (B2) relies on this: State() and PlayMove responses hand
// out snapshots cloned here, and the actor owns the single mutable original
// behind its command channel, so no lock is needed on the hot path.
func (gs *GameState) Clone() *GameState {
	if gs == nil {
		return nil
	}
	clone := &GameState{
		Board:          gs.Board,
		Chips:          make(map[Cell]Chip, len(gs.Chips)),
		Hands:          make(map[PlayerID][]Card, len(gs.Hands)),
		Draw:           append([]Card(nil), gs.Draw...),
		Discard:        append([]Card(nil), gs.Discard...),
		Turn:           gs.Turn,
		NumPlayers:     gs.NumPlayers,
		SequencesToWin: gs.SequencesToWin,
		SequencesWon:   make(map[PlayerID]int, len(gs.SequencesWon)),
		Sequences:      append([]Sequence(nil), gs.Sequences...),
		Winner:         gs.Winner,
		deadCardUsed:   gs.deadCardUsed,
	}
	for c, ch := range gs.Chips {
		clone.Chips[c] = ch
	}
	for p, hand := range gs.Hands {
		cp := make([]Card, len(hand))
		copy(cp, hand)
		clone.Hands[p] = cp
	}
	for p, n := range gs.SequencesWon {
		clone.SequencesWon[p] = n
	}
	return clone
}
