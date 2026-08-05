// Package engine is the pure, deterministic core of Tessera's Sequence
// implementation. It has no I/O, no networking, and no persistence — only
// game rules. That keeps it testable, race-free, and free of import cycles:
//
//	engine (pure) ← room manager ← transport ← persistence
//
// Every source of randomness flows in from the caller as a *rand.Rand
// derived from config.Config.NewRand, so the same seed reproduces an
// identical game.
package engine

import "fmt"

// Suit identifies a card suit.
type Suit int

const (
	SuitHearts   Suit = iota // ♥
	SuitDiamonds             // ♦
	SuitClubs                // ♣
	SuitSpades               // ♠
)

func (s Suit) String() string {
	switch s {
	case SuitHearts:
		return "H"
	case SuitDiamonds:
		return "D"
	case SuitClubs:
		return "C"
	case SuitSpades:
		return "S"
	default:
		return fmt.Sprintf("Suit(%d)", int(s))
	}
}

// Rank identifies a card rank.
type Rank int

const (
	RankAce   Rank = 1
	Rank2     Rank = 2
	Rank3     Rank = 3
	Rank4     Rank = 4
	Rank5     Rank = 5
	Rank6     Rank = 6
	Rank7     Rank = 7
	Rank8     Rank = 8
	Rank9     Rank = 9
	Rank10    Rank = 10
	RankJack  Rank = 11
	RankQueen Rank = 12
	RankKing  Rank = 13
)

func (r Rank) String() string {
	switch r {
	case RankAce:
		return "A"
	case RankJack:
		return "J"
	case RankQueen:
		return "Q"
	case RankKing:
		return "K"
	default:
		return fmt.Sprintf("%d", int(r))
	}
}

// Card is a single playing card (rank + suit).
type Card struct {
	Suit Suit `json:"suit"`
	Rank Rank `json:"rank"`
}

// String returns a compact representation like "AH", "10D", "JC".
func (c Card) String() string {
	return c.Rank.String() + c.Suit.String()
}

// IsJack reports whether c is any jack.
func IsJack(c Card) bool { return c.Rank == RankJack }

// IsTwoEyedJack reports whether c is a wild-placement jack (J♦ or J♣).
// These allow placing a chip on any open cell.
func IsTwoEyedJack(c Card) bool {
	return c.Rank == RankJack && (c.Suit == SuitDiamonds || c.Suit == SuitClubs)
}

// IsOneEyedJack reports whether c is a removal jack (J♥ or J♠).
// These allow removing one opponent chip that is not part of a completed
// sequence.
func IsOneEyedJack(c Card) bool {
	return c.Rank == RankJack && (c.Suit == SuitHearts || c.Suit == SuitSpades)
}
