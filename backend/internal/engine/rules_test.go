package engine

import (
	"errors"
	"math/rand/v2"
	"testing"
)

// controlled builds a 2-player game with a real board but empty hands/chips, so
// tests can script exact positions. Draw is left nil; set it per test when a
// replacement draw matters.
func controlled(seqToWin int) *GameState {
	return &GameState{
		Board:          NewBoard(testRand()),
		Chips:          make(map[Cell]Chip),
		Hands:          map[PlayerID][]Card{0: {}, 1: {}},
		Turn:           0,
		NumPlayers:     2,
		SequencesToWin: seqToWin,
		SequencesWon:   make(map[PlayerID]int),
		Winner:         NoPlayer,
	}
}

func TestNewGameDeal(t *testing.T) {
	gs, err := NewGame(testRand(), Options{NumPlayers: 2})
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	for p := PlayerID(0); p < 2; p++ {
		if len(gs.Hands[p]) != 7 {
			t.Errorf("player %d hand = %d cards, want 7", p, len(gs.Hands[p]))
		}
	}
	// 104 deck - 14 dealt = 90 remaining.
	if len(gs.Draw) != 90 {
		t.Errorf("draw pile = %d, want 90", len(gs.Draw))
	}
	if gs.Turn != 0 {
		t.Errorf("first turn = %d, want 0", gs.Turn)
	}
	if gs.GameOver() {
		t.Error("new game should not be over")
	}
	if gs.SequencesToWin != 2 {
		t.Errorf("default SequencesToWin = %d, want 2", gs.SequencesToWin)
	}
}

func TestNewGameDeterministic(t *testing.T) {
	a, _ := NewGame(testRand(), Options{NumPlayers: 2})
	b, _ := NewGame(testRand(), Options{NumPlayers: 2})
	for p := PlayerID(0); p < 2; p++ {
		for i := range a.Hands[p] {
			if a.Hands[p][i] != b.Hands[p][i] {
				t.Fatalf("hands not deterministic for player %d at %d", p, i)
			}
		}
	}
}

func TestNewGameValidation(t *testing.T) {
	if _, err := NewGame(testRand(), Options{NumPlayers: 1}); err == nil {
		t.Error("NumPlayers=1 should error")
	}
	if _, err := NewGame(testRand(), Options{NumPlayers: 5}); err == nil {
		t.Error("unsupported NumPlayers=5 should error")
	}
	gs, err := NewGame(testRand(), Options{NumPlayers: 2, SequencesToWin: 0})
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	if gs.SequencesToWin != 2 {
		t.Errorf("SequencesToWin=0 should default to 2, got %d", gs.SequencesToWin)
	}
}

func TestDeckExhaustionDrawRule(t *testing.T) {
	liveCell := Cell{Row: 4, Col: 4}
	cases := []struct {
		name  string
		setup func(*GameState)
		want  Outcome
	}{
		{
			name: "empty hand is stuck",
			setup: func(gs *GameState) {
				gs.Draw = nil
			},
			want: OutcomeDrawn,
		},
		{
			name: "cards still in deck",
			setup: func(gs *GameState) {
				gs.Draw = []Card{{Ace, Spades}}
			},
			want: OutcomeInProgress,
		},
		{
			name: "normal card can be placed",
			setup: func(gs *GameState) {
				card, _ := gs.Board.CardAt(liveCell)
				gs.Hands[0] = []Card{card}
			},
			want: OutcomeInProgress,
		},
		{
			name: "two eyed jack can be placed",
			setup: func(gs *GameState) {
				gs.Hands[0] = []Card{{Jack, Diamonds}}
			},
			want: OutcomeInProgress,
		},
		{
			name: "one eyed jack can remove",
			setup: func(gs *GameState) {
				gs.Hands[0] = []Card{{Jack, Hearts}}
				gs.Chips[liveCell] = Chip{Owner: 1}
			},
			want: OutcomeInProgress,
		},
		{
			name: "dead card cannot advance play",
			setup: func(gs *GameState) {
				card, _ := gs.Board.CardAt(liveCell)
				gs.Hands[0] = []Card{card}
				for _, cell := range gs.Board.CellsFor(card) {
					gs.Chips[cell] = Chip{Owner: 1}
				}
			},
			want: OutcomeDrawn,
		},
		{
			name: "locked chips cannot be removed",
			setup: func(gs *GameState) {
				gs.Hands[0] = []Card{{Jack, Hearts}}
				gs.Chips[liveCell] = Chip{Owner: 1, InSequence: true}
			},
			want: OutcomeDrawn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := controlled(999)
			tc.setup(gs)
			gs.markDrawIfStuck()
			if gs.Outcome != tc.want {
				t.Fatalf("outcome = %d, want %d", gs.Outcome, tc.want)
			}
			if got := gs.GameOver(); got != (tc.want != OutcomeInProgress) {
				t.Fatalf("GameOver = %t, want %t", got, tc.want != OutcomeInProgress)
			}
		})
	}
}

func TestFullDeckExhaustionEndsInDraw(t *testing.T) {
	gs, err := NewGame(rand.New(rand.NewPCG(1, 2)), Options{
		NumPlayers:     2,
		SequencesToWin: 999, // exercise exhaustion rather than the normal win path
	})
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}

	for moves := 0; !gs.GameOver(); moves++ {
		if moves >= 500 {
			t.Fatal("game did not terminate after consuming one deck")
		}
		move, ok := chooseLegalMove(gs)
		if !ok {
			t.Fatalf("player %d has no accepted action with %d cards still in the draw pile", gs.Turn, len(gs.Draw))
		}
		if err := gs.Apply(move); err != nil {
			t.Fatalf("move %d (%+v): %v", moves, move, err)
		}
	}

	if gs.Outcome != OutcomeDrawn || gs.Winner != NoPlayer {
		t.Fatalf("terminal result = outcome %d winner %d, want draw", gs.Outcome, gs.Winner)
	}
	if len(gs.Draw) != 0 {
		t.Fatalf("draw pile has %d cards, want empty", len(gs.Draw))
	}
	remaining := len(gs.Discard)
	for p := PlayerID(0); p < PlayerID(gs.NumPlayers); p++ {
		remaining += len(gs.Hands[p])
	}
	if remaining != len(NewDeck()) {
		t.Fatalf("cards accounted for = %d, want full %d-card deck", remaining, len(NewDeck()))
	}
	if err := gs.Apply(Move{Player: gs.Turn}); !errors.Is(err, ErrGameOver) {
		t.Fatalf("post-draw move err = %v, want %v", err, ErrGameOver)
	}
}

func chooseLegalMove(gs *GameState) (Move, bool) {
	p := gs.Turn
	for _, card := range gs.Hands[p] {
		switch {
		case card.IsTwoEyedJack():
			for row := 0; row < BoardSize; row++ {
				for col := 0; col < BoardSize; col++ {
					cell := Cell{Row: row, Col: col}
					if !gs.Board.IsCorner(cell) {
						if _, occupied := gs.Chips[cell]; !occupied {
							return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}, true
						}
					}
				}
			}
		case card.IsOneEyedJack():
			for row := 0; row < BoardSize; row++ {
				for col := 0; col < BoardSize; col++ {
					cell := Cell{Row: row, Col: col}
					if chip, occupied := gs.Chips[cell]; occupied && chip.Owner != p && !chip.InSequence {
						return Move{Player: p, Type: MoveRemove, Card: card, Cell: cell}, true
					}
				}
			}
		default:
			for _, cell := range gs.Board.CellsFor(card) {
				if _, occupied := gs.Chips[cell]; !occupied {
					return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}, true
				}
			}
		}
	}

	if !gs.deadCardUsed {
		for _, card := range gs.Hands[p] {
			if gs.isDead(card) {
				return Move{Player: p, Type: MoveDeadCard, Card: card}, true
			}
		}
	}
	return Move{}, false
}

func TestApplyPlaceNormalCard(t *testing.T) {
	gs := controlled(2)
	target := Cell{4, 4}
	card, _ := gs.Board.CardAt(target)
	replacement := Card{Queen, Clubs}
	gs.Hands[0] = []Card{card}
	gs.Draw = []Card{replacement}

	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: card, Cell: target}); err != nil {
		t.Fatalf("Apply place: %v", err)
	}
	if ch, ok := gs.Chips[target]; !ok || ch.Owner != 0 {
		t.Errorf("chip at %s = %+v, want owner 0", target, ch)
	}
	if len(gs.Hands[0]) != 1 || gs.Hands[0][0] != replacement {
		t.Errorf("hand after play = %v, want [%s] (card spent, replacement drawn)", gs.Hands[0], replacement)
	}
	if gs.Turn != 1 {
		t.Errorf("turn after play = %d, want 1", gs.Turn)
	}
	if len(gs.Discard) != 1 || gs.Discard[0] != card {
		t.Errorf("discard = %v, want [%s]", gs.Discard, card)
	}
}

func TestApplyPlaceErrors(t *testing.T) {
	target := Cell{4, 4}
	base := controlled(2)
	cardAtTarget, _ := base.Board.CardAt(target)

	cases := []struct {
		name  string
		setup func(gs *GameState) Move
		want  error
	}{
		{
			name: "out of turn",
			setup: func(gs *GameState) Move {
				gs.Hands[1] = []Card{cardAtTarget}
				return Move{Player: 1, Type: MovePlace, Card: cardAtTarget, Cell: target}
			},
			want: ErrNotYourTurn,
		},
		{
			name: "unknown player",
			setup: func(gs *GameState) Move {
				return Move{Player: 9, Type: MovePlace, Card: cardAtTarget, Cell: target}
			},
			want: ErrUnknownPlayer,
		},
		{
			name: "card not in hand",
			setup: func(gs *GameState) Move {
				return Move{Player: 0, Type: MovePlace, Card: cardAtTarget, Cell: target}
			},
			want: ErrCardNotInHand,
		},
		{
			name: "card/cell mismatch",
			setup: func(gs *GameState) Move {
				// A card whose real cells are elsewhere, played on `target`.
				other := Card{Ace, Spades}
				if other == cardAtTarget {
					other = Card{King, Hearts}
				}
				gs.Hands[0] = []Card{other}
				return Move{Player: 0, Type: MovePlace, Card: other, Cell: target}
			},
			want: ErrCardCellMismatch,
		},
		{
			name: "cell occupied",
			setup: func(gs *GameState) Move {
				gs.Chips[target] = Chip{Owner: 1}
				gs.Hands[0] = []Card{cardAtTarget}
				return Move{Player: 0, Type: MovePlace, Card: cardAtTarget, Cell: target}
			},
			want: ErrCellOccupied,
		},
		{
			name: "corner",
			setup: func(gs *GameState) Move {
				gs.Hands[0] = []Card{{Jack, Diamonds}}
				return Move{Player: 0, Type: MovePlace, Card: Card{Jack, Diamonds}, Cell: Cell{0, 0}}
			},
			want: ErrCellIsCorner,
		},
		{
			name: "out of bounds",
			setup: func(gs *GameState) Move {
				gs.Hands[0] = []Card{{Jack, Diamonds}}
				return Move{Player: 0, Type: MovePlace, Card: Card{Jack, Diamonds}, Cell: Cell{-1, 0}}
			},
			want: ErrCellOutOfBounds,
		},
		{
			name: "one-eyed jack cannot place",
			setup: func(gs *GameState) Move {
				gs.Hands[0] = []Card{{Jack, Hearts}}
				return Move{Player: 0, Type: MovePlace, Card: Card{Jack, Hearts}, Cell: target}
			},
			want: ErrJackNotPlaceable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := controlled(2)
			move := tc.setup(gs)
			if err := gs.Apply(move); !errors.Is(err, tc.want) {
				t.Fatalf("Apply err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestApplyRejectionLeavesStateUnchanged(t *testing.T) {
	gs := controlled(2)
	target := Cell{4, 4}
	gs.Chips[target] = Chip{Owner: 1}
	card, _ := gs.Board.CardAt(target)
	gs.Hands[0] = []Card{card}

	before := len(gs.Hands[0])
	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: card, Cell: target}); err == nil {
		t.Fatal("expected occupied-cell error")
	}
	// A rejected move must not spend the card or advance the turn.
	if len(gs.Hands[0]) != before {
		t.Errorf("hand changed after rejected move: %d != %d", len(gs.Hands[0]), before)
	}
	if gs.Turn != 0 {
		t.Errorf("turn advanced after rejected move: %d", gs.Turn)
	}
}

func TestTwoEyedJackPlacesAnywhere(t *testing.T) {
	gs := controlled(2)
	jack := Card{Jack, Diamonds}
	gs.Hands[0] = []Card{jack}
	// A cell that does NOT bear the jack (jacks are never on the board).
	target := Cell{3, 7}
	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: jack, Cell: target}); err != nil {
		t.Fatalf("two-eyed jack place: %v", err)
	}
	if ch, ok := gs.Chips[target]; !ok || ch.Owner != 0 {
		t.Errorf("chip at %s = %+v, want owner 0", target, ch)
	}
	if gs.Turn != 1 {
		t.Errorf("turn = %d, want 1", gs.Turn)
	}
}

func TestOneEyedJackRemove(t *testing.T) {
	gs := controlled(2)
	victim := Cell{4, 4}
	gs.Chips[victim] = Chip{Owner: 1}
	jack := Card{Jack, Hearts}
	gs.Hands[0] = []Card{jack}

	if err := gs.Apply(Move{Player: 0, Type: MoveRemove, Card: jack, Cell: victim}); err != nil {
		t.Fatalf("one-eyed jack remove: %v", err)
	}
	if _, ok := gs.Chips[victim]; ok {
		t.Errorf("chip at %s should have been removed", victim)
	}
	if gs.Turn != 1 {
		t.Errorf("turn = %d, want 1", gs.Turn)
	}
}

func TestOneEyedJackRemoveErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(gs *GameState) Move
		want  error
	}{
		{
			name: "empty cell",
			setup: func(gs *GameState) Move {
				gs.Hands[0] = []Card{{Jack, Spades}}
				return Move{Player: 0, Type: MoveRemove, Card: Card{Jack, Spades}, Cell: Cell{4, 4}}
			},
			want: ErrNotRemovable,
		},
		{
			name: "own chip",
			setup: func(gs *GameState) Move {
				gs.Chips[Cell{4, 4}] = Chip{Owner: 0}
				gs.Hands[0] = []Card{{Jack, Spades}}
				return Move{Player: 0, Type: MoveRemove, Card: Card{Jack, Spades}, Cell: Cell{4, 4}}
			},
			want: ErrNotRemovable,
		},
		{
			name: "locked chip",
			setup: func(gs *GameState) Move {
				gs.Chips[Cell{4, 4}] = Chip{Owner: 1, InSequence: true}
				gs.Hands[0] = []Card{{Jack, Spades}}
				return Move{Player: 0, Type: MoveRemove, Card: Card{Jack, Spades}, Cell: Cell{4, 4}}
			},
			want: ErrNotRemovable,
		},
		{
			name: "not a one-eyed jack",
			setup: func(gs *GameState) Move {
				gs.Chips[Cell{4, 4}] = Chip{Owner: 1}
				gs.Hands[0] = []Card{{Jack, Diamonds}}
				return Move{Player: 0, Type: MoveRemove, Card: Card{Jack, Diamonds}, Cell: Cell{4, 4}}
			},
			want: ErrNotOneEyedJack,
		},
		{
			name: "corner",
			setup: func(gs *GameState) Move {
				gs.Hands[0] = []Card{{Jack, Spades}}
				return Move{Player: 0, Type: MoveRemove, Card: Card{Jack, Spades}, Cell: Cell{0, 0}}
			},
			want: ErrCellIsCorner,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := controlled(2)
			move := tc.setup(gs)
			if err := gs.Apply(move); !errors.Is(err, tc.want) {
				t.Fatalf("Apply err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDeadCardSwap(t *testing.T) {
	gs := controlled(2)
	dead, _ := gs.Board.CardAt(Cell{4, 4})
	// Occupy both board cells of `dead` so it can never be played.
	for _, c := range gs.Board.CellsFor(dead) {
		gs.Chips[c] = Chip{Owner: 1}
	}
	replacement := Card{Queen, Clubs}
	gs.Hands[0] = []Card{dead}
	gs.Draw = []Card{replacement}

	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: dead}); err != nil {
		t.Fatalf("dead-card swap: %v", err)
	}
	if len(gs.Hands[0]) != 1 || gs.Hands[0][0] != replacement {
		t.Errorf("hand after swap = %v, want [%s]", gs.Hands[0], replacement)
	}
	// Dead-card swap does NOT end the turn.
	if gs.Turn != 0 {
		t.Errorf("turn = %d, want 0 (swap keeps the turn)", gs.Turn)
	}

	// A second swap in the same turn is rejected, even with another dead card.
	dead2, _ := gs.Board.CardAt(Cell{5, 5})
	for _, c := range gs.Board.CellsFor(dead2) {
		gs.Chips[c] = Chip{Owner: 1}
	}
	gs.Hands[0] = []Card{dead2}
	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: dead2}); !errors.Is(err, ErrDeadCardUsed) {
		t.Fatalf("second swap err = %v, want %v", err, ErrDeadCardUsed)
	}
}

func TestDeadCardRejectsLiveCard(t *testing.T) {
	gs := controlled(2)
	// A card with at least one open cell is not dead.
	live, _ := gs.Board.CardAt(Cell{4, 4})
	gs.Hands[0] = []Card{live}
	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: live}); !errors.Is(err, ErrCardNotDead) {
		t.Fatalf("live-card swap err = %v, want %v", err, ErrCardNotDead)
	}

	// A jack is never dead.
	gs.Hands[0] = []Card{{Jack, Diamonds}}
	if err := gs.Apply(Move{Player: 0, Type: MoveDeadCard, Card: Card{Jack, Diamonds}}); !errors.Is(err, ErrCardNotDead) {
		t.Fatalf("jack swap err = %v, want %v", err, ErrCardNotDead)
	}
}

func TestPlaceRecordsSequenceWithoutWinning(t *testing.T) {
	gs := controlled(2) // needs 2 sequences to win
	// Pre-place four chips on row 5, cols 1..4; the winning cell is (5,5).
	gs.place(0, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
	winCard, _ := gs.Board.CardAt(Cell{5, 5})
	gs.Hands[0] = []Card{winCard}
	gs.Draw = []Card{{Ace, Spades}, {Queen, Clubs}}

	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: winCard, Cell: Cell{5, 5}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if gs.SequencesWon[0] != 1 {
		t.Errorf("SequencesWon[0] = %d, want 1", gs.SequencesWon[0])
	}
	if gs.GameOver() {
		t.Error("game should not be over with 1 of 2 sequences")
	}
	if gs.Turn != 1 {
		t.Errorf("turn = %d, want 1 (game continues)", gs.Turn)
	}
	assertLocked(t, gs, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4}, Cell{5, 5})
}

func TestWinCondition(t *testing.T) {
	gs := controlled(1) // one sequence wins
	gs.place(0, Cell{5, 1}, Cell{5, 2}, Cell{5, 3}, Cell{5, 4})
	jack := Card{Jack, Clubs} // two-eyed jack completes the run at (5,5)
	gs.Hands[0] = []Card{jack}
	gs.Draw = []Card{{Queen, Clubs}}

	if err := gs.Apply(Move{Player: 0, Type: MovePlace, Card: jack, Cell: Cell{5, 5}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if gs.Winner != 0 {
		t.Fatalf("winner = %d, want 0", gs.Winner)
	}
	if gs.Outcome != OutcomeWon {
		t.Fatalf("outcome = %d, want won", gs.Outcome)
	}
	if !gs.GameOver() {
		t.Error("game should be over")
	}
	// On a win the turn does not advance.
	if gs.Turn != 0 {
		t.Errorf("turn = %d, want 0 after win", gs.Turn)
	}
	// Any further move is rejected.
	gs.Hands[1] = []Card{{Jack, Diamonds}}
	if err := gs.Apply(Move{Player: 1, Type: MovePlace, Card: Card{Jack, Diamonds}, Cell: Cell{6, 6}}); !errors.Is(err, ErrGameOver) {
		t.Fatalf("post-win move err = %v, want %v", err, ErrGameOver)
	}
}

// TestFullGameSmoke drives a complete game with greedy legal play and asserts no
// move is ever rejected and the game terminates cleanly. It exercises the whole
// engine against a real deal across several seeds.
func TestFullGameSmoke(t *testing.T) {
	seeds := []uint64{1, 2, 3, 42, 1234}
	for _, seed := range seeds {
		gs, err := NewGame(rngFrom(seed), Options{NumPlayers: 2, SequencesToWin: 1})
		if err != nil {
			t.Fatalf("seed %d: NewGame: %v", seed, err)
		}
		moves := 0
		for !gs.GameOver() {
			m, ok := greedyMove(gs)
			if !ok {
				break // no legal move (e.g. stalemate/empty draw); acceptable
			}
			if err := gs.Apply(m); err != nil {
				t.Fatalf("seed %d move %d: unexpected Apply error: %v", seed, moves, err)
			}
			moves++
			if moves > 5000 {
				t.Fatalf("seed %d: game did not terminate in 5000 moves", seed)
			}
		}
		// Every chip's owner must be a valid seat.
		for cell, ch := range gs.Chips {
			if !gs.knownPlayer(ch.Owner) {
				t.Errorf("seed %d: chip at %s has invalid owner %d", seed, cell, ch.Owner)
			}
		}
	}
}

// greedyMove returns any legal move for the player on turn, preferring a normal
// placement, then a two-eyed jack, then a one-eyed jack removal.
func greedyMove(gs *GameState) (Move, bool) {
	p := gs.Turn
	open := func(c Cell) bool {
		_, occ := gs.Chips[c]
		return !occ && !gs.Board.IsCorner(c)
	}
	for _, card := range gs.Hands[p] {
		switch {
		case card.IsTwoEyedJack():
			for r := 0; r < BoardSize; r++ {
				for c := 0; c < BoardSize; c++ {
					if open(Cell{r, c}) {
						return Move{Player: p, Type: MovePlace, Card: card, Cell: Cell{r, c}}, true
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
				if open(cell) {
					return Move{Player: p, Type: MovePlace, Card: card, Cell: cell}, true
				}
			}
		}
	}
	return Move{}, false
}
