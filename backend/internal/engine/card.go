// Package engine is the pure, I/O-free core of Tessera: the rules of Sequence.
//
// Layering (see project.prompt): the engine is the innermost layer. It must not
// import networking, persistence, or any other application package — only the
// standard library. Everything here is deterministic: given the same injected
// *rand.Rand and the same sequence of moves, the engine produces byte-identical
// state on every run. That is what makes games reproducible from a single seed
// and lets the durability layer (B4) replay a log to rebuild state exactly.
//
// "Pure" here means: no I/O, no hidden global state, and no randomness that is
// not injected. State-changing operations validate fully before mutating, so a
// rejected move never leaves the state half-changed (transactional semantics the
// room manager in B2 relies on for idempotency).
package engine

import (
	"math/rand/v2"
)

// Suit is one of the four card suits. The suit also decides what a jack does:
// diamonds/clubs jacks are two-eyed (wild), hearts/spades jacks are one-eyed
// (remove an opponent's chip). See Card.IsTwoEyedJack / IsOneEyedJack.
type Suit uint8

const (
	Spades Suit = iota
	Hearts
	Diamonds
	Clubs
)

// Rank is a card's rank. Values are contiguous starting at Ace so ranks can be
// ranged over and used as slice indices during deck construction.
type Rank uint8

const (
	Ace Rank = iota
	Two
	Three
	Four
	Five
	Six
	Seven
	Eight
	Nine
	Ten
	Jack
	Queen
	King
)

// Card is a single playing card. It is a comparable value type, so it can be a
// map key (used for the board's card→cells index) and compared with ==.
type Card struct {
	Rank Rank
	Suit Suit
}

// suitRunes and rankStrings drive the compact String forms used in logs, test
// failure messages, and (eventually) the JSON the client renders.
var suitRunes = [...]rune{Spades: '♠', Hearts: '♥', Diamonds: '♦', Clubs: '♣'}

var rankStrings = [...]string{
	Ace: "A", Two: "2", Three: "3", Four: "4", Five: "5", Six: "6",
	Seven: "7", Eight: "8", Nine: "9", Ten: "10", Jack: "J", Queen: "Q", King: "K",
}

// String renders a card like "A♠" or "10♦".
func (c Card) String() string {
	r := "?"
	if int(c.Rank) < len(rankStrings) {
		r = rankStrings[c.Rank]
	}
	s := '?'
	if int(c.Suit) < len(suitRunes) {
		s = suitRunes[c.Suit]
	}
	return r + string(s)
}

// IsJack reports whether the card is any jack.
func (c Card) IsJack() bool { return c.Rank == Jack }

// IsTwoEyedJack reports whether the card is a two-eyed (wild) jack: J♦ or J♣.
// A two-eyed jack lets a player place a chip on ANY open cell.
func (c Card) IsTwoEyedJack() bool {
	return c.Rank == Jack && (c.Suit == Diamonds || c.Suit == Clubs)
}

// IsOneEyedJack reports whether the card is a one-eyed jack: J♥ or J♠.
// A one-eyed jack lets a player REMOVE one opponent chip that is not already
// locked into a completed sequence.
func (c Card) IsOneEyedJack() bool {
	return c.Rank == Jack && (c.Suit == Hearts || c.Suit == Spades)
}

// allSuits and allNonJackRanks are the building blocks for a single 52-card
// deck. Non-jack ranks are the 48 cards that appear on the board (twice each).
var allSuits = [...]Suit{Spades, Hearts, Diamonds, Clubs}

var allRanks = [...]Rank{
	Ace, Two, Three, Four, Five, Six, Seven, Eight, Nine, Ten, Jack, Queen, King,
}

// NewDeck returns two full 52-card decks combined (104 cards), in a fixed
// canonical order. Sequence is played with a double deck: 104 draw cards, of
// which the 96 non-jack cards mirror the 96 non-corner board cells (48 unique
// non-jack cards × 2). The 8 jacks are the wild/remove action cards.
//
// The order is deterministic; callers shuffle with an injected *rand.Rand.
func NewDeck() []Card {
	deck := make([]Card, 0, 104)
	for copies := 0; copies < 2; copies++ {
		for _, s := range allSuits {
			for _, r := range allRanks {
				deck = append(deck, Card{Rank: r, Suit: s})
			}
		}
	}
	return deck
}

// Shuffle performs an in-place Fisher–Yates shuffle using the injected RNG, so
// the result is deterministic for a given (seed, stream). The engine never
// creates its own randomness — reproducibility depends on this.
func Shuffle(rng *rand.Rand, cards []Card) {
	for i := len(cards) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		cards[i], cards[j] = cards[j], cards[i]
	}
}
