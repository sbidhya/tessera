// Package engine — clone support for WAL pre-validation.
package engine

import (
	"maps"
	"slices"
)

// Clone returns a deep copy of the game state. The board is immutable and
// shared; every map and slice is copied so the clone can be mutated without
// affecting the original.
func (gs *GameState) Clone() *GameState {
	clone := &GameState{
		Board:          gs.Board,
		Chips:          maps.Clone(gs.Chips),
		Hands:          make(map[PlayerID][]Card, len(gs.Hands)),
		Draw:           slices.Clone(gs.Draw),
		Discard:        slices.Clone(gs.Discard),
		Turn:           gs.Turn,
		NumPlayers:     gs.NumPlayers,
		SequencesToWin: gs.SequencesToWin,
		SequencesWon:   maps.Clone(gs.SequencesWon),
		Sequences:      slices.Clone(gs.Sequences),
		Winner:         gs.Winner,
		deadCardUsed:   gs.deadCardUsed,
	}
	for p, hand := range gs.Hands {
		clone.Hands[p] = slices.Clone(hand)
	}
	return clone
}
