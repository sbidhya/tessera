package engine

import (
	"errors"
	"testing"
)

// TestDrawTrigger covers the exact draw rule: the draw pile is empty AND the
// player to move has no legal move. Each case scripts one move from a
// controlled position and checks whether the game is drawn afterwards.
func TestDrawTrigger(t *testing.T) {
	placeAt := func(gs *GameState, p PlayerID, cell Cell) Move {
		card, _ := gs.Board.CardAt(cell)
		gs.Hands[p] = []Card{card}
		return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}
	}

	cases := []struct {
		name string
		// setup returns the game and the move to apply.
		setup func(gs *GameState) Move
		// wantDraw is whether the game must be drawn after the move.
		wantDraw bool
		// wantWinner is the expected winner after the move (NoPlayer for draws).
		wantWinner PlayerID
	}{
		{
			name: "empty pile and opponent out of cards draws",
			setup: func(gs *GameState) Move {
				gs.Draw = nil
				return placeAt(gs, 0, Cell{4, 4})
			},
			wantDraw:   true,
			wantWinner: NoPlayer,
		},
		{
			name: "non-empty pile continues despite empty opponent hand",
			setup: func(gs *GameState) Move {
				// Two cards so the pile survives the replacement draw.
				gs.Draw = []Card{{King, Hearts}, {Queen, Clubs}}
				return placeAt(gs, 0, Cell{4, 4})
			},
			wantDraw:   false,
			wantWinner: NoPlayer,
		},
		{
			name: "empty pile but opponent holds a playable card continues",
			setup: func(gs *GameState) Move {
				gs.Draw = nil
				live, _ := gs.Board.CardAt(Cell{6, 6})
				gs.Hands[1] = []Card{live}
				return placeAt(gs, 0, Cell{4, 4})
			},
			wantDraw:   false,
			wantWinner: NoPlayer,
		},
		{
			name: "empty pile and opponent holds only a stuck one-eyed jack draws",
			setup: func(gs *GameState) Move {
				gs.Draw = nil
				// Player 0 completes a (non-winning) sequence, locking every
				// chip on the board. A bare placement would leave its own
				// chip removable, so the lock is what strands player 1's
				// one-eyed jack with no legal removal.
				gs.place(0, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
				gs.Hands[1] = []Card{{Jack, Hearts}}
				return placeAt(gs, 0, Cell{5, 5})
			},
			wantDraw:   true,
			wantWinner: NoPlayer,
		},
		{
			name: "empty pile but a removable chip keeps the game alive",
			setup: func(gs *GameState) Move {
				gs.Draw = nil
				// An unlocked chip owned by player 0 gives player 1's
				// one-eyed jack a legal removal.
				gs.Chips[Cell{1, 1}] = Chip{Owner: 0}
				gs.Hands[1] = []Card{{Jack, Hearts}}
				return placeAt(gs, 0, Cell{4, 4})
			},
			wantDraw:   false,
			wantWinner: NoPlayer,
		},
		{
			name: "empty pile and a full board with only a wild jack draws",
			setup: func(gs *GameState) Move {
				gs.Draw = nil
				// Fill every non-corner cell but one with the opponent's
				// chips, so player 0's placement wins no sequence (they own
				// a single chip) while the board ends up completely full.
				target := Cell{4, 4}
				for row := 0; row < BoardSize; row++ {
					for col := 0; col < BoardSize; col++ {
						cell := Cell{Row: row, Col: col}
						if gs.Board.IsCorner(cell) || cell == target {
							continue
						}
						gs.Chips[cell] = Chip{Owner: 1}
					}
				}
				gs.Hands[0] = []Card{{Jack, Diamonds}}
				gs.Hands[1] = []Card{{Jack, Clubs}}
				return Move{Player: 0, Type: MovePlace, Card: Card{Jack, Diamonds}, Cell: target}
			},
			wantDraw:   true,
			wantWinner: NoPlayer,
		},
		{
			name: "winning move that empties the pile is a win not a draw",
			setup: func(gs *GameState) Move {
				gs.SequencesToWin = 1
				gs.place(0, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
				gs.Draw = []Card{{Queen, Clubs}}
				return placeAt(gs, 0, Cell{5, 5})
			},
			wantDraw:   false,
			wantWinner: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := controlled(2)
			move := tc.setup(gs)
			if err := gs.Apply(move); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if gs.IsDraw() != tc.wantDraw {
				t.Errorf("IsDraw = %v, want %v", gs.IsDraw(), tc.wantDraw)
			}
			if gs.Winner != tc.wantWinner {
				t.Errorf("Winner = %d, want %d", gs.Winner, tc.wantWinner)
			}
			if tc.wantDraw && !gs.GameOver() {
				t.Error("GameOver = false after a draw, want true")
			}
			if !tc.wantDraw && tc.wantWinner == NoPlayer && gs.GameOver() {
				t.Error("GameOver = true, want false (game should continue)")
			}
		})
	}
}

// TestDeadSwapIntoDraw checks the turn-stays variant: a dead-card exchange
// with an empty pile that leaves the same player with no legal move draws
// without advancing the turn.
func TestDeadSwapIntoDraw(t *testing.T) {
	gs := controlled(2)
	gs.Draw = nil
	dead, _ := gs.Board.CardAt(Cell{4, 4})
	for _, c := range gs.Board.CellsFor(dead) {
		gs.Chips[c] = Chip{Owner: 1}
	}
	gs.Hands[0] = []Card{dead}

	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: dead}); err != nil {
		t.Fatalf("dead-card swap: %v", err)
	}
	if !gs.IsDraw() {
		t.Fatal("game should be drawn after swapping the last card with an empty pile")
	}
	if gs.Turn != 0 {
		t.Errorf("turn = %d, want 0 (swap keeps the turn, draw freezes it)", gs.Turn)
	}
}

// TestDeadSwapWithLiveHandContinues ensures a dead-card exchange is not itself
// terminal when the player still holds a playable card.
func TestDeadSwapWithLiveHandContinues(t *testing.T) {
	gs := controlled(2)
	gs.Draw = nil
	dead, _ := gs.Board.CardAt(Cell{4, 4})
	for _, c := range gs.Board.CellsFor(dead) {
		gs.Chips[c] = Chip{Owner: 1}
	}
	live, _ := gs.Board.CardAt(Cell{6, 6})
	gs.Hands[0] = []Card{dead, live}

	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: dead}); err != nil {
		t.Fatalf("dead-card swap: %v", err)
	}
	if gs.GameOver() {
		t.Fatal("game should continue: the player still holds a playable card")
	}
}

// TestMovesAfterDrawRejected pins the terminal behaviour: once drawn, every
// move fails with ErrGameOver and the state is unchanged.
func TestMovesAfterDrawRejected(t *testing.T) {
	gs := controlled(2)
	gs.Draw = nil
	card, _ := gs.Board.CardAt(Cell{4, 4})
	gs.Hands[0] = []Card{card}
	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: card, Cell: Cell{4, 4}}); err != nil {
		t.Fatalf("setup move: %v", err)
	}
	if !gs.IsDraw() {
		t.Fatal("setup did not produce a draw")
	}
	chips := len(gs.Chips)
	gs.Hands[1] = []Card{{Jack, Diamonds}}
	err := gs.Apply(Move{Player: 1, Type: MovePlace, Card: Card{Jack, Diamonds}, Cell: Cell{3, 3}})
	if !errors.Is(err, ErrGameOver) {
		t.Fatalf("post-draw move err = %v, want %v", err, ErrGameOver)
	}
	if len(gs.Chips) != chips {
		t.Error("rejected post-draw move mutated the board")
	}
}

// TestTerminalStates documents how Winner and Drawn combine: NoPlayer alone
// means "in progress" — only an explicit marker ends the game.
func TestTerminalStates(t *testing.T) {
	gs := controlled(2)
	if gs.GameOver() || gs.IsDraw() {
		t.Fatal("fresh game must be in progress")
	}
	gs.Winner = 1
	if !gs.GameOver() || gs.IsDraw() {
		t.Fatal("win must be terminal without being a draw")
	}
	gs.Winner = NoPlayer
	gs.Drawn = true
	if !gs.GameOver() || !gs.IsDraw() {
		t.Fatal("draw must be terminal with no winner")
	}
}

// TestDrawCardAceOfSpades pins the empty-pile sentinel fix: Card{} is the Ace
// of Spades, a real card, so drawing it must still deal it to the hand.
func TestDrawCardAceOfSpades(t *testing.T) {
	gs := controlled(2)
	target := Cell{4, 4}
	card, _ := gs.Board.CardAt(target)
	gs.Hands[0] = []Card{card}
	gs.Draw = []Card{{Ace, Spades}}

	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: card, Cell: target}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(gs.Hands[0]) != 1 || gs.Hands[0][0] != (Card{Ace, Spades}) {
		t.Errorf("hand after drawing A♠ = %v, want [A♠]", gs.Hands[0])
	}
}

// exhaustMove returns a legal move for the player on turn, including dead-card
// swaps, so a full-deck game can be driven to its true terminal state.
func exhaustMove(gs *GameState) (Move, bool) {
	p := gs.Turn
	for _, card := range gs.Hands[p] {
		switch {
		case card.IsTwoEyedJack():
			for r := 0; r < BoardSize; r++ {
				for c := 0; c < BoardSize; c++ {
					cell := Cell{Row: r, Col: c}
					if _, occ := gs.Chips[cell]; !occ && !gs.Board.IsCorner(cell) {
						return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}, true
					}
				}
			}
		case card.IsOneEyedJack():
			for cell, ch := range gs.Chips {
				if ch.Owner != p && !ch.InSequence {
					return Move{Player: p, Type: MoveRemove, Card: card, Cell: cell}, true
				}
			}
		default:
			for _, cell := range gs.Board.CellsFor(card) {
				if _, occ := gs.Chips[cell]; !occ {
					return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}, true
				}
			}
		}
	}
	for _, card := range gs.Hands[p] {
		if card.IsJack() {
			continue
		}
		cells := gs.Board.CellsFor(card)
		if len(cells) == 0 {
			continue
		}
		dead := true
		for _, c := range cells {
			if _, occ := gs.Chips[c]; !occ {
				dead = false
				break
			}
		}
		if dead {
			return Move{Player: p, Type: MoveDeadCard, Card: card}, true
		}
	}
	return Move{}, false
}

// TestFullDeckExhaustionIsDraw plays complete decks with wins disabled and
// requires every game to terminate in a draw — the regression test for matches
// stuck in StatusPlaying forever once the pile runs out.
func TestFullDeckExhaustionIsDraw(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 7, 42, 1234} {
		gs, err := NewGame(rngFrom(seed), Options{NumPlayers: 2, SequencesToWin: 100})
		if err != nil {
			t.Fatalf("seed %d: NewGame: %v", seed, err)
		}
		moves := 0
		for !gs.GameOver() {
			m, ok := exhaustMove(gs)
			if !ok {
				t.Fatalf("seed %d: no legal move after %d moves with pile=%d hands=%d/%d, but game is not over",
					seed, moves, len(gs.Draw), len(gs.Hands[0]), len(gs.Hands[1]))
			}
			if err := gs.Apply(m); err != nil {
				t.Fatalf("seed %d move %d: unexpected Apply error: %v", seed, moves, err)
			}
			moves++
			if moves > 5000 {
				t.Fatalf("seed %d: game did not terminate in 5000 moves", seed)
			}
		}
		if !gs.IsDraw() {
			t.Errorf("seed %d: GameOver without a draw (winner=%d) after full-deck play", seed, gs.Winner)
		}
		if gs.Winner != NoPlayer {
			t.Errorf("seed %d: drawn game has winner %d, want NoPlayer", seed, gs.Winner)
		}
		if len(gs.Draw) != 0 {
			t.Errorf("seed %d: drawn game still has %d cards in the pile", seed, len(gs.Draw))
		}
		t.Logf("seed %d: draw after %d moves, hands=%d/%d chips=%d",
			seed, moves, len(gs.Hands[0]), len(gs.Hands[1]), len(gs.Chips))
	}
}
