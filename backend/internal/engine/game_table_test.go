package engine

import (
	"errors"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
)

// TestValidateMoveTable is the crown-jewel table that exercises every illegal
// and legal move shape the engine must enforce. Each entry isolates one rule
// so a regression is immediately attributable.
func TestValidateMoveTable(t *testing.T) {
	base := func() *GameState {
		gs := newTestGame(t, 99, GameConfig{HandSize: 4, SequencesToWin: 2})
		gs.CurrentTurn = 0
		// Reset board to empty to avoid dead-card side effects from dealt hands.
		for r := 0; r < BoardSize; r++ {
			for c := 0; c < BoardSize; c++ {
				gs.Chips[r][c] = chipEmpty
			}
		}
		gs.Locked = LockedGrid(gs.Chips, 2)
		return gs
	}

	// Precompute a normal card and its board cell for reuse.
	normalCardForCell := func(gs *GameState, r, c int) Card {
		card, _ := gs.Board.CardAt(r, c)
		return card
	}

	tests := []struct {
		name    string
		setup   func(gs *GameState) Move
		wantErr error
		valid   bool
	}{
		{
			name: "legal normal placement",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 1, 1)
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 1, Col: 1}
			},
			valid: true,
		},
		{
			name: "out of turn",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 1, 1)
				gs.Players[1].Hand = []Card{card}
				return Move{PlayerID: "bob", Card: card, Row: 1, Col: 1}
			},
			wantErr: ErrOutOfTurn,
		},
		{
			name: "card not in hand",
			setup: func(gs *GameState) Move {
				gs.Players[0].Hand = []Card{{SuitHearts, Rank2}}
				card := normalCardForCell(gs, 1, 1) // AH, not in hand
				return Move{PlayerID: "alice", Card: card, Row: 1, Col: 1}
			},
			wantErr: ErrCardNotInHand,
		},
		{
			name: "card does not match cell",
			setup: func(gs *GameState) Move {
				gs.Players[0].Hand = []Card{{SuitHearts, Rank2}}
				// Board at 1,1 is something else (deterministic), but 2H likely not there.
				// If by chance 1,1 is 2H, use a different mismatched card.
				mismatch := Card{SuitHearts, Rank2}
				boardCard, _ := gs.Board.CardAt(1, 1)
				if boardCard == mismatch {
					mismatch = Card{SuitSpades, RankKing}
					gs.Players[0].Hand = []Card{mismatch}
				}
				return Move{PlayerID: "alice", Card: mismatch, Row: 1, Col: 1}
			},
			wantErr: ErrCardDoesNotMatchCell,
		},
		{
			name: "cell occupied",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 2, 2)
				gs.Players[0].Hand = []Card{card}
				gs.Chips[2][2] = 1
				return Move{PlayerID: "alice", Card: card, Row: 2, Col: 2}
			},
			wantErr: ErrCellOccupied,
		},
		{
			name: "corner not playable normal",
			setup: func(gs *GameState) Move {
				card := Card{SuitHearts, RankAce}
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 0, Col: 0}
			},
			wantErr: ErrCornerNotPlayable,
		},
		{
			name: "corner not playable two-eyed jack",
			setup: func(gs *GameState) Move {
				j := Card{SuitDiamonds, RankJack}
				gs.Players[0].Hand = []Card{j}
				return Move{PlayerID: "alice", Card: j, Row: 0, Col: 9}
			},
			wantErr: ErrCornerNotPlayable,
		},
		{
			name: "out of bounds negative",
			setup: func(gs *GameState) Move {
				card := Card{SuitHearts, RankAce}
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: -1, Col: 0}
			},
			wantErr: ErrCellOutOfBounds,
		},
		{
			name: "out of bounds high",
			setup: func(gs *GameState) Move {
				card := Card{SuitHearts, RankAce}
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 10, Col: 10}
			},
			wantErr: ErrCellOutOfBounds,
		},
		{
			name: "two-eyed jack legal wild",
			setup: func(gs *GameState) Move {
				j := Card{SuitClubs, RankJack}
				gs.Players[0].Hand = []Card{j}
				return Move{PlayerID: "alice", Card: j, Row: 5, Col: 5}
			},
			valid: true,
		},
		{
			name: "two-eyed jack occupied fails",
			setup: func(gs *GameState) Move {
				j := Card{SuitClubs, RankJack}
				gs.Players[0].Hand = []Card{j}
				gs.Chips[5][5] = 1
				return Move{PlayerID: "alice", Card: j, Row: 5, Col: 5}
			},
			wantErr: ErrCellOccupied,
		},
		{
			name: "one-eyed jack legal remove",
			setup: func(gs *GameState) Move {
				j := Card{SuitHearts, RankJack}
				gs.Players[0].Hand = []Card{j}
				gs.Chips[4][4] = 1
				return Move{PlayerID: "alice", Card: j, Row: 4, Col: 4}
			},
			valid: true,
		},
		{
			name: "one-eyed jack remove empty fails",
			setup: func(gs *GameState) Move {
				j := Card{SuitHearts, RankJack}
				gs.Players[0].Hand = []Card{j}
				return Move{PlayerID: "alice", Card: j, Row: 4, Col: 4}
			},
			wantErr: ErrCellEmpty,
		},
		{
			name: "one-eyed jack remove own fails",
			setup: func(gs *GameState) Move {
				j := Card{SuitSpades, RankJack}
				gs.Players[0].Hand = []Card{j}
				gs.Chips[4][4] = 0
				return Move{PlayerID: "alice", Card: j, Row: 4, Col: 4}
			},
			wantErr: ErrCannotRemoveOwnChip,
		},
		{
			name: "one-eyed jack remove locked fails",
			setup: func(gs *GameState) Move {
				j := Card{SuitSpades, RankJack}
				gs.Players[0].Hand = []Card{j}
				// Make a locked sequence for bob
				for c := 1; c <= 5; c++ {
					gs.Chips[6][c] = 1
				}
				gs.Locked = LockedGrid(gs.Chips, 2)
				return Move{PlayerID: "alice", Card: j, Row: 6, Col: 3}
			},
			wantErr: ErrCannotRemoveLockedChip,
		},
		{
			name: "dead card legal",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 0, 1)
				pos := gs.Board.PositionsFor(card)
				gs.Chips[pos[0].Row][pos[0].Col] = 0
				gs.Chips[pos[1].Row][pos[1].Col] = 1
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, IsDiscard: true}
			},
			valid: true,
		},
		{
			name: "dead card not dead fails",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 0, 1)
				pos := gs.Board.PositionsFor(card)
				gs.Chips[pos[0].Row][pos[0].Col] = 0
				// leave second empty
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, IsDiscard: true}
			},
			wantErr: ErrNotDeadCard,
		},
		{
			name: "dead card jack fails",
			setup: func(gs *GameState) Move {
				j := Card{SuitDiamonds, RankJack}
				gs.Players[0].Hand = []Card{j}
				return Move{PlayerID: "alice", Card: j, IsDiscard: true}
			},
			wantErr: ErrJackCannotBeDead,
		},
		{
			name: "game over blocks",
			setup: func(gs *GameState) Move {
				w := 0
				gs.Winner = &w
				card := normalCardForCell(gs, 1, 1)
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 1, Col: 1}
			},
			wantErr: ErrGameOver,
		},
		{
			name: "player not found",
			setup: func(gs *GameState) Move {
				card := normalCardForCell(gs, 1, 1)
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "nobody", Card: card, Row: 1, Col: 1}
			},
			wantErr: ErrPlayerNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := base()
			move := tc.setup(gs)
			err := gs.ValidateMove(move)
			if tc.valid {
				if err != nil {
					t.Fatalf("expected valid, got err %v", err)
				}
				// Also check ApplyMove succeeds
				if _, err := gs.ApplyMove(move); err != nil {
					t.Fatalf("ApplyMove expected valid, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected err %v, got nil", tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v want %v", err, tc.wantErr)
				}
				if _, err := gs.ApplyMove(move); !errors.Is(err, tc.wantErr) {
					t.Fatalf("ApplyMove err = %v want %v", err, tc.wantErr)
				}
			}
		})
	}
}

func TestWinConditionsTable(t *testing.T) {
	tests := []struct {
		name           string
		sequencesToWin int
		setup          func(gs *GameState) // mutates chips to have N-1 sequences
		winningMove    func(gs *GameState) Move
		wantWinner     int
	}{
		{
			name:           "single sequence win with 1 to win",
			sequencesToWin: 1,
			setup: func(gs *GameState) {
				for c := 1; c <= 4; c++ {
					gs.Chips[5][c] = 0
				}
			},
			winningMove: func(gs *GameState) Move {
				card, _ := gs.Board.CardAt(5, 5)
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 5, Col: 5}
			},
			wantWinner: 0,
		},
		{
			name:           "second sequence win with 2 to win",
			sequencesToWin: 2,
			setup: func(gs *GameState) {
				for c := 1; c <= 5; c++ {
					gs.Chips[2][c] = 0
				}
				for c := 1; c <= 4; c++ {
					gs.Chips[8][c] = 0
				}
			},
			winningMove: func(gs *GameState) Move {
				card, _ := gs.Board.CardAt(8, 5)
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 8, Col: 5}
			},
			wantWinner: 0,
		},
		{
			name:           "bob wins vertical",
			sequencesToWin: 1,
			setup: func(gs *GameState) {
				for r := 1; r <= 4; r++ {
					gs.Chips[r][8] = 1
				}
			},
			winningMove: func(gs *GameState) Move {
				card, _ := gs.Board.CardAt(5, 8)
				gs.Players[1].Hand = []Card{card}
				gs.CurrentTurn = 1
				return Move{PlayerID: "bob", Card: card, Row: 5, Col: 8}
			},
			wantWinner: 1,
		},
		{
			name:           "win does not happen prematurely",
			sequencesToWin: 2,
			setup: func(gs *GameState) {
				// Only one sequence, placing unrelated chip should not win
				for c := 1; c <= 5; c++ {
					gs.Chips[2][c] = 0
				}
			},
			winningMove: func(gs *GameState) Move {
				// Place somewhere not forming second sequence
				card, _ := gs.Board.CardAt(9, 1)
				// Ensure that placement doesn't make a sequence
				gs.Players[0].Hand = []Card{card}
				return Move{PlayerID: "alice", Card: card, Row: 9, Col: 1}
			},
			wantWinner: -1, // no winner
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Seed: 1}
			rng := cfg.NewRand("test")
			gs, _ := NewGame([]string{"alice", "bob"}, rng, GameConfig{SequencesToWin: tc.sequencesToWin, HandSize: 3})
			// Reset to empty for deterministic setup
			for r := 0; r < BoardSize; r++ {
				for c := 0; c < BoardSize; c++ {
					gs.Chips[r][c] = chipEmpty
				}
			}
			gs.CurrentTurn = 0
			tc.setup(gs)
			gs.Locked = LockedGrid(gs.Chips, 2)
			gs.Sequences = []int{len(FindSequences(gs.Chips, 0)), len(FindSequences(gs.Chips, 1))}
			move := tc.winningMove(gs)
			// Ensure current turn matches move player for valid test; adjust if needed
			if idx, ok := gs.PlayerIndex(move.PlayerID); ok {
				gs.CurrentTurn = idx
			}
			next, err := gs.ApplyMove(move)
			if err != nil {
				t.Fatalf("ApplyMove failed: %v", err)
			}
			if tc.wantWinner == -1 {
				if next.Winner != nil {
					t.Fatalf("unexpected winner %v", *next.Winner)
				}
			} else {
				if next.Winner == nil || *next.Winner != tc.wantWinner {
					t.Fatalf("winner = %v want %d, seqs=%v", next.Winner, tc.wantWinner, next.Sequences)
				}
			}
		})
	}
}

func TestTwoEyedJackCoversFullBoard(t *testing.T) {
	// Verify two-eyed jack can be placed on any non-corner empty cell, even if
	// the board card doesn't match (wild).
	gs := newTestGame(t, 5, GameConfig{})
	gs.CurrentTurn = 0
	jack := Card{SuitDiamonds, RankJack}
	gs.Players[0].Hand = []Card{jack}
	// Try every open cell; pick a few representative ones.
	cases := []Position{{1, 1}, {5, 5}, {9, 1}, {0, 5}, {8, 8}}
	for _, p := range cases {
		gs.Chips[p.Row][p.Col] = chipEmpty
		move := Move{PlayerID: "alice", Card: jack, Row: p.Row, Col: p.Col}
		if err := gs.ValidateMove(move); err != nil {
			t.Errorf("two-eyed jack wild at %v should be valid: %v", p, err)
		}
	}
}

func TestOneEyedJackRemovesAnyOpponentChip(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	// Place bob chips in multiple locations
	for _, p := range []Position{{2, 2}, {5, 5}, {8, 1}} {
		gs.Chips[p.Row][p.Col] = 1
	}
	gs.Locked = LockedGrid(gs.Chips, 2)
	for _, p := range []Position{{2, 2}, {5, 5}, {8, 1}} {
		gs.Players[0].Hand = []Card{{SuitHearts, RankJack}}
		move := Move{PlayerID: "alice", Card: Card{SuitHearts, RankJack}, Row: p.Row, Col: p.Col}
		if err := gs.ValidateMove(move); err != nil {
			t.Errorf("one-eyed remove at %v should be valid: %v", p, err)
		}
	}
}
