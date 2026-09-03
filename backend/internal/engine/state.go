package engine

import (
	"fmt"
	"math/rand/v2"
)

// PlayerID identifies a player by seat index, 0..NumPlayers-1.
type PlayerID int

// NoPlayer is the zero-value sentinel for "no player" (e.g. no winner yet).
const NoPlayer PlayerID = -1

// Outcome is the terminal result of a match. OutcomeInProgress is the zero
// value so a GameState cannot accidentally look finished before play starts.
type Outcome uint8

const (
	OutcomeInProgress Outcome = iota
	OutcomeWon
	OutcomeDrawn
)

// Chip is a marker a player has placed on a cell.
type Chip struct {
	// Owner is the player who placed the chip.
	Owner PlayerID
	// InSequence is true once the chip becomes part of a completed 5-in-a-row.
	// Locked chips cannot be removed by a one-eyed jack — this is the rule that
	// makes finished sequences permanent.
	InSequence bool
}

// Sequence is a completed run of five cells (a scoring line) owned by a player.
// Corners may appear as members because they are wild.
type Sequence struct {
	Owner PlayerID
	Cells [5]Cell
}

// Options configures a new game.
type Options struct {
	// NumPlayers is the number of seats. v1 supports 2.
	NumPlayers int
	// SequencesToWin is how many completed sequences a player needs to win.
	// Default 2; 1 makes for fast test games.
	SequencesToWin int
}

// handSize maps player count to the number of cards dealt to each hand, per the
// standard Sequence rules. v1 targets 2 players.
var handSize = map[int]int{2: 7, 3: 6, 4: 6, 6: 5}

// GameState is the complete, authoritative state of one match. It is owned by a
// single goroutine in the room manager (B2); the engine never shares it across
// goroutines, so no locking lives here.
type GameState struct {
	Board *Board

	// Chips holds only occupied cells (corners are never present — they are wild,
	// not chipped).
	Chips map[Cell]Chip

	// Hands[p] is player p's current hand.
	Hands map[PlayerID][]Card

	// Draw is the draw pile; the top card is the last element (popped from the
	// end for O(1) draws). Discard collects spent and dead cards.
	Draw    []Card
	Discard []Card

	// Turn is whose turn it is. Advances after a place/remove move.
	Turn PlayerID

	NumPlayers     int
	SequencesToWin int

	// SequencesWon counts completed sequences per player. Sequences records the
	// actual lines, used to enforce the "share at most one cell" overlap rule.
	SequencesWon map[PlayerID]int
	Sequences    []Sequence

	// Outcome distinguishes an unfinished match from a terminal draw. A draw is
	// declared after the deck is empty and the player whose turn it is has no
	// legal place or removal. We use that rule instead of waiting for every hand
	// to be empty: dead cards and unusable one-eyed jacks cannot advance play, so
	// requiring empty hands could still leave a match stuck forever.
	Outcome Outcome

	// Winner is the winning player, or NoPlayer unless Outcome is OutcomeWon.
	Winner PlayerID

	// deadCardUsed tracks the once-per-turn dead-card swap allowance.
	deadCardUsed bool
}

// NewGame deals a fresh game from the injected RNG. The board layout and deck
// shuffle are both derived from rng in a fixed order, so the entire game is
// reproducible from the seed behind rng.
func NewGame(rng *rand.Rand, opts Options) (*GameState, error) {
	if opts.NumPlayers < 2 {
		return nil, fmt.Errorf("engine: NumPlayers must be >= 2, got %d", opts.NumPlayers)
	}
	hs, ok := handSize[opts.NumPlayers]
	if !ok {
		return nil, fmt.Errorf("engine: unsupported NumPlayers %d", opts.NumPlayers)
	}
	if opts.SequencesToWin < 1 {
		opts.SequencesToWin = 2
	}

	gs := &GameState{
		Board:          NewBoard(rng),
		Chips:          make(map[Cell]Chip),
		Hands:          make(map[PlayerID][]Card, opts.NumPlayers),
		Turn:           0,
		NumPlayers:     opts.NumPlayers,
		SequencesToWin: opts.SequencesToWin,
		SequencesWon:   make(map[PlayerID]int, opts.NumPlayers),
		Outcome:        OutcomeInProgress,
		Winner:         NoPlayer,
	}

	// Deck is built after the board so both draw from the same stream in a fixed
	// order; the board pool and the draw deck are independent card sets.
	deck := NewDeck()
	Shuffle(rng, deck)
	gs.Draw = deck

	// Deal hs cards to each player, round-robin (matching how cards come off a
	// real shuffled deck).
	for i := 0; i < hs; i++ {
		for p := 0; p < opts.NumPlayers; p++ {
			card, _ := gs.drawCard()
			gs.Hands[PlayerID(p)] = append(gs.Hands[PlayerID(p)], card)
		}
	}

	return gs, nil
}

// GameOver reports whether the game has ended in either a win or a draw.
func (gs *GameState) GameOver() bool { return gs.Outcome != OutcomeInProgress }

// drawCard pops the top card off the draw pile. The boolean is false when the
// pile is empty. Card{} cannot be an empty sentinel because it is the valid ace
// of spades.
func (gs *GameState) drawCard() (Card, bool) {
	n := len(gs.Draw)
	if n == 0 {
		return Card{}, false
	}
	card := gs.Draw[n-1]
	gs.Draw = gs.Draw[:n-1]
	return card, true
}

// handIndex returns the index of card in player p's hand, or -1 if absent.
func (gs *GameState) handIndex(p PlayerID, card Card) int {
	for i, c := range gs.Hands[p] {
		if c == card {
			return i
		}
	}
	return -1
}

// removeFromHand removes the card at index i from player p's hand.
func (gs *GameState) removeFromHand(p PlayerID, i int) {
	hand := gs.Hands[p]
	gs.Hands[p] = append(hand[:i], hand[i+1:]...)
}

// nextTurn advances to the next seat and resets the per-turn dead-card allowance.
func (gs *GameState) nextTurn() {
	gs.Turn = PlayerID((int(gs.Turn) + 1) % gs.NumPlayers)
	gs.deadCardUsed = false
}

// knownPlayer reports whether p is a valid seat in this game.
func (gs *GameState) knownPlayer(p PlayerID) bool {
	return p >= 0 && int(p) < gs.NumPlayers
}
