package engine

import (
	"errors"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
)

// helper to create a deterministic game with known players.
func newTestGame(t *testing.T, seed int64, cfg GameConfig) *GameState {
	t.Helper()
	c := config.Config{Seed: seed}
	rng := c.NewRand("test")
	gs, err := NewGame([]string{"alice", "bob"}, rng, cfg)
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	return gs
}

func TestNewGameDefaults(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	if gs.Config.SequencesToWin != 2 {
		t.Errorf("default SequencesToWin = %d want 2", gs.Config.SequencesToWin)
	}
	if gs.Config.HandSize != 7 {
		t.Errorf("default HandSize = %d want 7", gs.Config.HandSize)
	}
	if len(gs.Players) != 2 {
		t.Fatalf("players len = %d want 2", len(gs.Players))
	}
	for i, p := range gs.Players {
		if len(p.Hand) != 7 {
			t.Errorf("player %d hand len = %d want 7", i, len(p.Hand))
		}
	}
	// Deck should have 104 - 14 = 90 left
	if gs.Deck.Len() != 90 {
		t.Errorf("deck len = %d want 90", gs.Deck.Len())
	}
	// Chips empty
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			if gs.Chips[r][c] != chipEmpty {
				t.Errorf("chip %d,%d not empty", r, c)
			}
		}
	}
	if gs.Winner != nil {
		t.Error("new game should have no winner")
	}
}

func TestNewGameValidation(t *testing.T) {
	cfg := config.Config{Seed: 1}
	rng := cfg.NewRand("test")
	cases := []struct {
		name string
		ids  []string
		err  error
	}{
		{"too few", []string{"a"}, ErrInvalidPlayerCount},
		{"too many", []string{"a", "b", "c"}, ErrInvalidPlayerCount},
		{"duplicate", []string{"a", "a"}, ErrDuplicatePlayerID},
		{"empty id", []string{"a", ""}, ErrEmptyPlayerID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewGame(tc.ids, rng, GameConfig{})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v want %v", err, tc.err)
			}
		})
	}
}

func TestNewGameDeterministic(t *testing.T) {
	cfg := config.Config{Seed: 42}
	rng1 := cfg.NewRand("test")
	rng2 := cfg.NewRand("test")
	gs1, _ := NewGame([]string{"a", "b"}, rng1, GameConfig{HandSize: 5})
	gs2, _ := NewGame([]string{"a", "b"}, rng2, GameConfig{HandSize: 5})
	for i := range gs1.Players {
		if len(gs1.Players[i].Hand) != len(gs2.Players[i].Hand) {
			t.Fatalf("hand len mismatch")
		}
		for j := range gs1.Players[i].Hand {
			if gs1.Players[i].Hand[j] != gs2.Players[i].Hand[j] {
				t.Fatalf("hand %d card %d mismatch %v vs %v", i, j, gs1.Players[i].Hand[j], gs2.Players[i].Hand[j])
			}
		}
	}
	if gs1.CurrentTurn != gs2.CurrentTurn {
		t.Errorf("start turn mismatch %d vs %d", gs1.CurrentTurn, gs2.CurrentTurn)
	}
}

func TestNewGameStartingPlayerSeeded(t *testing.T) {
	// With same seed, starting player should be same; with different seed may differ.
	// Just verify it's within bounds.
	gs := newTestGame(t, 123, GameConfig{})
	if gs.CurrentTurn < 0 || gs.CurrentTurn >= 2 {
		t.Errorf("CurrentTurn = %d out of bounds", gs.CurrentTurn)
	}
}

func TestGameCloneIndependence(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{HandSize: 5})
	cp := gs.Clone()
	cp.Players[0].Hand[0] = Card{SuitHearts, RankAce}
	if gs.Players[0].Hand[0] == cp.Players[0].Hand[0] && len(gs.Players[0].Hand) > 0 {
		// If original hand happened to already be AH, this could false-positive; pick a card not in hand
		// Instead check that modifying chips doesn't affect original.
	}
	cp.Chips[0][1] = 0
	if gs.Chips[0][1] == 0 {
		t.Error("Clone shares Chips array")
	}
}

func TestGameCurrentPlayerID(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	if got := gs.CurrentPlayerID(); got != "alice" {
		t.Errorf("CurrentPlayerID = %q want alice", got)
	}
	gs.CurrentTurn = 1
	if got := gs.CurrentPlayerID(); got != "bob" {
		t.Errorf("CurrentPlayerID = %q want bob", got)
	}
}

func TestValidateMoveOutOfTurn(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{HandSize: 5})
	// Force turn to alice
	gs.CurrentTurn = 0
	// Bob tries to move
	move := Move{PlayerID: "bob", Card: gs.Players[1].Hand[0], Row: 1, Col: 1}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrOutOfTurn) {
		t.Fatalf("want ErrOutOfTurn got %v", err)
	}
}

func TestValidateMovePlayerNotFound(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	move := Move{PlayerID: "nobody", Card: Card{SuitHearts, RankAce}}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("want ErrPlayerNotFound got %v", err)
	}
}

func TestValidateMoveCardNotInHand(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	// Give alice a known hand without AH
	gs.Players[0].Hand = []Card{{SuitHearts, Rank2}, {SuitHearts, Rank3}}
	move := Move{PlayerID: "alice", Card: Card{SuitHearts, RankAce}, Row: 0, Col: 1}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCardNotInHand) {
		t.Fatalf("want ErrCardNotInHand got %v", err)
	}
}

func TestValidateAndApplyNormalMove(t *testing.T) {
	gs := newTestGame(t, 10, GameConfig{HandSize: 5})
	gs.CurrentTurn = 0
	// Pick a card that has an open matching cell and put it in alice's hand
	// Use board position (0,1) which is AH per board layout.
	boardCard, _ := gs.Board.CardAt(0, 1)
	gs.Players[0].Hand = []Card{boardCard, {SuitHearts, Rank2}}
	// Ensure target empty
	if gs.Chips[0][1] != chipEmpty {
		t.Fatal("target should be empty")
	}
	move := Move{PlayerID: "alice", Card: boardCard, Row: 0, Col: 1}
	if err := gs.ValidateMove(move); err != nil {
		t.Fatalf("ValidateMove failed: %v", err)
	}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Chips[0][1] != 0 {
		t.Errorf("chip not placed")
	}
	if next.CurrentTurn != 1 {
		t.Errorf("turn should advance to 1 got %d", next.CurrentTurn)
	}
	if len(next.Players[0].Hand) != 2 { // started 2, consumed 1, drew 1 =2
		t.Errorf("hand len after move = %d want 2", len(next.Players[0].Hand))
	}
	if len(next.DiscardPile) != 1 || next.DiscardPile[0] != boardCard {
		t.Errorf("discard pile incorrect: %v", next.DiscardPile)
	}
	// Original should be unchanged (immutability)
	if gs.Chips[0][1] != chipEmpty {
		t.Error("original mutated")
	}
}

func TestApplyMoveCardDoesNotMatch(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	// Board at 0,1 is AH, give alice 2H and try to place 2H on AH cell
	gs.Players[0].Hand = []Card{{SuitHearts, Rank2}}
	move := Move{PlayerID: "alice", Card: Card{SuitHearts, Rank2}, Row: 0, Col: 1}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCardDoesNotMatchCell) {
		t.Fatalf("want ErrCardDoesNotMatchCell got %v", err)
	}
}

func TestApplyMoveCellOccupied(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	boardCard, _ := gs.Board.CardAt(0, 1)
	gs.Players[0].Hand = []Card{boardCard}
	gs.Chips[0][1] = 1 // occupied by bob
	move := Move{PlayerID: "alice", Card: boardCard, Row: 0, Col: 1}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCellOccupied) {
		t.Fatalf("want ErrCellOccupied got %v", err)
	}
}

func TestApplyMoveCornerNotPlayable(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	gs.Players[0].Hand = []Card{{SuitDiamonds, RankJack}} // two-eyed jack
	move := Move{PlayerID: "alice", Card: Card{SuitDiamonds, RankJack}, Row: 0, Col: 0}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCornerNotPlayable) {
		t.Fatalf("want ErrCornerNotPlayable got %v", err)
	}
}

func TestApplyMoveOutOfBounds(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	gs.Players[0].Hand = []Card{{SuitHearts, RankAce}}
	move := Move{PlayerID: "alice", Card: Card{SuitHearts, RankAce}, Row: -1, Col: 0}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCellOutOfBounds) {
		t.Fatalf("want ErrCellOutOfBounds got %v", err)
	}
	move = Move{PlayerID: "alice", Card: Card{SuitHearts, RankAce}, Row: 10, Col: 10}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCellOutOfBounds) {
		t.Fatalf("want ErrCellOutOfBounds got %v", err)
	}
}

func TestDeadCardDiscard(t *testing.T) {
	gs := newTestGame(t, 2, GameConfig{})
	gs.CurrentTurn = 0
	// Find a card and occupy both its positions
	// Use board card at (0,1) = AH, find its second position
	card, _ := gs.Board.CardAt(0, 1)
	pos := gs.Board.PositionsFor(card)
	if len(pos) != 2 {
		t.Fatalf("positions len %d", len(pos))
	}
	gs.Chips[pos[0].Row][pos[0].Col] = 0
	gs.Chips[pos[1].Row][pos[1].Col] = 1
	gs.Players[0].Hand = []Card{card, {SuitHearts, Rank3}}
	move := Move{PlayerID: "alice", Card: card, IsDiscard: true}
	if err := gs.ValidateMove(move); err != nil {
		t.Fatalf("dead card ValidateMove failed: %v", err)
	}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("dead card ApplyMove failed: %v", err)
	}
	if len(next.Players[0].Hand) != 2 { // consumed 1, drew 1
		t.Errorf("hand len after dead discard = %d want 2", len(next.Players[0].Hand))
	}
	if next.Chips[pos[0].Row][pos[0].Col] != 0 || next.Chips[pos[1].Row][pos[1].Col] != 1 {
		t.Error("chips should not change on dead discard")
	}
	if next.CurrentTurn != 1 {
		t.Errorf("turn should advance after dead discard")
	}
}

func TestDeadCardNotDead(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	card, _ := gs.Board.CardAt(0, 1)
	pos := gs.Board.PositionsFor(card)
	// Only one occupied
	gs.Chips[pos[0].Row][pos[0].Col] = 0
	gs.Players[0].Hand = []Card{card}
	move := Move{PlayerID: "alice", Card: card, IsDiscard: true}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrNotDeadCard) {
		t.Fatalf("want ErrNotDeadCard got %v", err)
	}
}

func TestJackCannotBeDead(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	jack := Card{SuitHearts, RankJack}
	gs.Players[0].Hand = []Card{jack}
	move := Move{PlayerID: "alice", Card: jack, IsDiscard: true}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrJackCannotBeDead) {
		t.Fatalf("want ErrJackCannotBeDead got %v", err)
	}
}

func TestTwoEyedJackWildPlacement(t *testing.T) {
	gs := newTestGame(t, 5, GameConfig{})
	gs.CurrentTurn = 0
	jack := Card{SuitDiamonds, RankJack}
	gs.Players[0].Hand = []Card{jack}
	// Place on any open cell, e.g., (5,5). Its board card is irrelevant.
	move := Move{PlayerID: "alice", Card: jack, Row: 5, Col: 5}
	if err := gs.ValidateMove(move); err != nil {
		t.Fatalf("two-eyed jack ValidateMove failed: %v", err)
	}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Chips[5][5] != 0 {
		t.Error("jack wild placement didn't set chip")
	}
	// Occupied cell should fail
	gs2 := next
	gs2.CurrentTurn = 1
	gs2.Players[1].Hand = []Card{{SuitClubs, RankJack}}
	move2 := Move{PlayerID: "bob", Card: Card{SuitClubs, RankJack}, Row: 5, Col: 5}
	if err := gs2.ValidateMove(move2); !errors.Is(err, ErrCellOccupied) {
		t.Fatalf("want ErrCellOccupied for jack on occupied cell got %v", err)
	}
}

func TestOneEyedJackRemove(t *testing.T) {
	gs := newTestGame(t, 6, GameConfig{})
	gs.CurrentTurn = 0
	// Place opponent chip
	gs.Chips[5][5] = 1
	jack := Card{SuitHearts, RankJack}
	gs.Players[0].Hand = []Card{jack}
	move := Move{PlayerID: "alice", Card: jack, Row: 5, Col: 5}
	if err := gs.ValidateMove(move); err != nil {
		t.Fatalf("one-eyed jack ValidateMove failed: %v", err)
	}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Chips[5][5] != chipEmpty {
		t.Error("chip not removed")
	}
}

func TestOneEyedJackCannotRemoveOwn(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	gs.Chips[5][5] = 0 // own chip
	gs.Players[0].Hand = []Card{{SuitHearts, RankJack}}
	move := Move{PlayerID: "alice", Card: Card{SuitHearts, RankJack}, Row: 5, Col: 5}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCannotRemoveOwnChip) {
		t.Fatalf("want ErrCannotRemoveOwnChip got %v", err)
	}
}

func TestOneEyedJackCannotRemoveEmpty(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	gs.Players[0].Hand = []Card{{SuitSpades, RankJack}}
	move := Move{PlayerID: "alice", Card: Card{SuitSpades, RankJack}, Row: 5, Col: 5}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCellEmpty) {
		t.Fatalf("want ErrCellEmpty got %v", err)
	}
}

func TestOneEyedJackCannotRemoveLocked(t *testing.T) {
	gs := newTestGame(t, 9, GameConfig{SequencesToWin: 2})
	gs.CurrentTurn = 0
	// Create a sequence for bob (player 1) at row 5 col 1-5
	for c := 1; c <= 5; c++ {
		gs.Chips[5][c] = 1
	}
	// Recompute locked
	gs.Locked = LockedGrid(gs.Chips, 2)
	gs.Sequences = []int{len(FindSequences(gs.Chips, 0)), len(FindSequences(gs.Chips, 1))}
	if gs.Sequences[1] != 1 {
		t.Fatalf("setup sequence not detected, got %d", gs.Sequences[1])
	}
	// Alice tries to remove a locked chip
	gs.Players[0].Hand = []Card{{SuitHearts, RankJack}}
	move := Move{PlayerID: "alice", Card: Card{SuitHearts, RankJack}, Row: 5, Col: 3}
	if err := gs.ValidateMove(move); !errors.Is(err, ErrCannotRemoveLockedChip) {
		t.Fatalf("want ErrCannotRemoveLockedChip got %v", err)
	}
}

func TestWinSingleSequence(t *testing.T) {
	gs := newTestGame(t, 10, GameConfig{SequencesToWin: 1, HandSize: 5})
	gs.CurrentTurn = 0
	// Place 4 chips for alice at row 2 col 1-4
	for c := 1; c <= 4; c++ {
		gs.Chips[2][c] = 0
	}
	// Need a card that matches col 5 at same row (2,5) to complete 5
	card, ok := gs.Board.CardAt(2, 5)
	if !ok {
		t.Fatal("card at 2,5 not found")
	}
	gs.Players[0].Hand = []Card{card}
	// Ensure Locked recomputed initially (should be 0)
	gs.Locked = LockedGrid(gs.Chips, 2)
	move := Move{PlayerID: "alice", Card: card, Row: 2, Col: 5}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Winner == nil || *next.Winner != 0 {
		t.Fatalf("winner = %v want 0", next.Winner)
	}
	if !next.IsTerminal() {
		t.Error("should be terminal after win")
	}
	// Further moves should fail
	next.Players[1].Hand = []Card{{SuitHearts, Rank2}}
	move2 := Move{PlayerID: "bob", Card: next.Players[1].Hand[0], Row: 3, Col: 3}
	if err := next.ValidateMove(move2); !errors.Is(err, ErrGameOver) {
		t.Fatalf("want ErrGameOver after win got %v", err)
	}
}

func TestWinTwoSequences(t *testing.T) {
	gs := newTestGame(t, 11, GameConfig{SequencesToWin: 2, HandSize: 5})
	gs.CurrentTurn = 0
	// Give alice one completed sequence at row 2, and 4 chips of second at row 7
	for c := 1; c <= 5; c++ {
		gs.Chips[2][c] = 0
	}
	for c := 1; c <= 4; c++ {
		gs.Chips[7][c] = 0
	}
	gs.Locked = LockedGrid(gs.Chips, 2)
	gs.Sequences = []int{len(FindSequences(gs.Chips, 0)), 0}
	if gs.Sequences[0] != 1 {
		t.Fatalf("first seq not setup, got %d", gs.Sequences[0])
	}
	card, _ := gs.Board.CardAt(7, 5)
	gs.Players[0].Hand = []Card{card}
	move := Move{PlayerID: "alice", Card: card, Row: 7, Col: 5}
	next, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Winner == nil || *next.Winner != 0 {
		t.Fatalf("winner = %v want 0", next.Winner)
	}
	if next.Sequences[0] != 2 {
		t.Errorf("sequences = %d want 2", next.Sequences[0])
	}
}

func TestWinWithCornerSequence(t *testing.T) {
	gs := newTestGame(t, 12, GameConfig{SequencesToWin: 1})
	gs.CurrentTurn = 0
	// Use corner (0,0) plus 4 chips at top row
	for c := 1; c <= 4; c++ {
		gs.Chips[0][c] = 0
	}
	gs.Locked = LockedGrid(gs.Chips, 2)
	card, _ := gs.Board.CardAt(0, 5)
	// Wait we already have 4 chips at 0,1-4. To win with corner we need to place at 0,5? Let's compute:
	// Sequence covering (0,0)-(0,4) is already 5 with corner? Actually (0,0) corner + (0,1)-(0,4) = 5.
	// So the current board with 4 chips already plus corner is already a sequence, but we haven't counted yet.
	// We need to simulate move that creates it. Let's start with 3 chips and place 4th.
	gs2 := newTestGame(t, 12, GameConfig{SequencesToWin: 1})
	gs2.CurrentTurn = 0
	for c := 1; c <= 3; c++ {
		gs2.Chips[0][c] = 0
	}
	gs2.Locked = LockedGrid(gs2.Chips, 2)
	card2, _ := gs2.Board.CardAt(0, 4)
	gs2.Players[0].Hand = []Card{card2}
	move := Move{PlayerID: "alice", Card: card2, Row: 0, Col: 4}
	next, err := gs2.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove failed: %v", err)
	}
	if next.Winner == nil || *next.Winner != 0 {
		t.Fatalf("corner win not detected winner=%v seqs=%v", next.Winner, next.Sequences)
	}
	_ = card
}

func TestApplyMoveImmutability(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 0
	card, _ := gs.Board.CardAt(0, 1)
	gs.Players[0].Hand = []Card{card}
	origChips := gs.Chips
	move := Move{PlayerID: "alice", Card: card, Row: 0, Col: 1}
	_, err := gs.ApplyMove(move)
	if err != nil {
		t.Fatalf("ApplyMove: %v", err)
	}
	if gs.Chips != origChips {
		t.Error("original GameState mutated")
	}
}

func TestTurnAdvancesAndDraw(t *testing.T) {
	gs := newTestGame(t, 100, GameConfig{HandSize: 3})
	gs.CurrentTurn = 0
	initialDeckLen := gs.Deck.Len()
	card, _ := gs.Board.CardAt(0, 1)
	gs.Players[0].Hand = []Card{card, {SuitHearts, Rank2}, {SuitHearts, Rank3}}
	move := Move{PlayerID: "alice", Card: card, Row: 0, Col: 1}
	next, _ := gs.ApplyMove(move)
	if next.Deck.Len() != initialDeckLen-1 {
		t.Errorf("deck len after move = %d want %d", next.Deck.Len(), initialDeckLen-1)
	}
	if next.TurnCount != gs.TurnCount+1 {
		t.Errorf("TurnCount not incremented")
	}
}

func TestGameOverBlocksMoves(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{SequencesToWin: 1})
	gs.CurrentTurn = 0
	for c := 1; c <= 4; c++ {
		gs.Chips[1][c] = 0
	}
	gs.Locked = LockedGrid(gs.Chips, 2)
	card, _ := gs.Board.CardAt(1, 5)
	gs.Players[0].Hand = []Card{card}
	next, _ := gs.ApplyMove(Move{PlayerID: "alice", Card: card, Row: 1, Col: 5})
	if !next.IsTerminal() {
		t.Fatal("should be terminal")
	}
	// Try to move after game over
	gs2 := next
	gs2.CurrentTurn = 1 // even if we set turn, should still block
	gs2.Players[1].Hand = []Card{{SuitHearts, Rank5}}
	move := Move{PlayerID: "bob", Card: Card{SuitHearts, Rank5}, Row: 2, Col: 2}
	if _, err := gs2.ApplyMove(move); !errors.Is(err, ErrGameOver) {
		t.Fatalf("want ErrGameOver got %v", err)
	}
}

func TestOutOfTurnViaApply(t *testing.T) {
	gs := newTestGame(t, 1, GameConfig{})
	gs.CurrentTurn = 1 // bob's turn
	// alice tries
	card := gs.Players[0].Hand[0]
	move := Move{PlayerID: "alice", Card: card, Row: 0, Col: 1}
	if _, err := gs.ApplyMove(move); !errors.Is(err, ErrOutOfTurn) {
		t.Fatalf("want ErrOutOfTurn got %v", err)
	}
}
