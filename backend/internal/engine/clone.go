package engine

import (
	"maps"
	"slices"
)

// Clone returns a deep copy that can be mutated independently. Board is shared
// because it is immutable after construction; every mutable map and slice is
// copied, including the slices stored inside Hands.
//
// The room actor uses this to prepare a legal move before writing its WAL
// record. Only after the append succeeds does the prepared state become
// authoritative, giving the order validate -> persist -> apply -> acknowledge.
func (gs *GameState) Clone() *GameState {
	clone := *gs
	clone.Chips = maps.Clone(gs.Chips)
	clone.Hands = make(map[PlayerID][]Card, len(gs.Hands))
	for player, hand := range gs.Hands {
		clone.Hands[player] = slices.Clone(hand)
	}
	clone.Draw = slices.Clone(gs.Draw)
	clone.Discard = slices.Clone(gs.Discard)
	clone.SequencesWon = maps.Clone(gs.SequencesWon)
	clone.Sequences = slices.Clone(gs.Sequences)
	return &clone
}
