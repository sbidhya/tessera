package engine

import (
	"math/rand/v2"
	"testing"
)

func TestGameStateCloneIsIndependent(t *testing.T) {
	original, err := NewGame(rand.New(rand.NewPCG(1, 2)), Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	clone := original.Clone()

	cell := Cell{Row: 1, Col: 1}
	clone.Chips[cell] = Chip{Owner: 0}
	clone.Hands[0][0].Rank = Rank((int(clone.Hands[0][0].Rank) + 1) % len(allRanks))
	clone.Draw[0].Rank = Rank((int(clone.Draw[0].Rank) + 1) % len(allRanks))
	clone.Discard = append(clone.Discard, Card{Rank: Queen, Suit: Clubs})
	clone.SequencesWon[0] = 9
	clone.Sequences = append(clone.Sequences, Sequence{Owner: 0})

	if _, ok := original.Chips[cell]; ok {
		t.Error("clone Chips aliases original")
	}
	if original.Hands[0][0] == clone.Hands[0][0] {
		t.Error("clone Hands aliases original")
	}
	if original.Draw[0] == clone.Draw[0] {
		t.Error("clone Draw aliases original")
	}
	if len(original.Discard) != 0 {
		t.Error("clone Discard aliases original")
	}
	if original.SequencesWon[0] != 0 {
		t.Error("clone SequencesWon aliases original")
	}
	if len(original.Sequences) != 0 {
		t.Error("clone Sequences aliases original")
	}
	if clone.Board != original.Board {
		t.Error("immutable Board should be shared")
	}
}
