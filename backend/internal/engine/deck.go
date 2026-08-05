package engine

import "math/rand/v2"

// Deck is a stack of cards. The "top" of the deck is the end of the slice
// so Draw is O(1) and shuffling can use rand.Rand.Shuffle directly.
type Deck []Card

// NewDeck returns a freshly shuffled double deck (104 cards, two copies of
// each of the 52 standard cards). Shuffle order is driven by rng so the
// same (seed, stream) reproduces an identical deck. rng must be non-nil.
//
// Tessera's board uses only the 48 non-jack cards, but the deck must contain
// jacks because they are playable actions (wild / removal). Sequence is
// traditionally played with two decks, which also keeps the draw pile large
// enough for a full game (14 dealt + up to ~90 placements).
func NewDeck(rng *rand.Rand) Deck {
	if rng == nil {
		panic("engine: NewDeck requires non-nil rng (use Config.NewRand)")
	}
	cards := make(Deck, 0, 104)
	for copy := 0; copy < 2; copy++ {
		for s := SuitHearts; s <= SuitSpades; s++ {
			for r := RankAce; r <= RankKing; r++ {
				cards = append(cards, Card{Suit: s, Rank: r})
			}
		}
	}
	cards.Shuffle(rng)
	return cards
}

// Shuffle randomises the deck in place using rng. The method exists so
// callers (and tests) can re-shuffle an existing deck without allocating.
func (d Deck) Shuffle(rng *rand.Rand) {
	rng.Shuffle(len(d), func(i, j int) { d[i], d[j] = d[j], d[i] })
}

// Draw removes and returns the top card. The second result is false if the
// deck is empty.
func (d *Deck) Draw() (Card, bool) {
	if len(*d) == 0 {
		return Card{}, false
	}
	n := len(*d) - 1
	c := (*d)[n]
	*d = (*d)[:n]
	return c, true
}

// Len returns the number of cards remaining.
func (d Deck) Len() int { return len(d) }

// Clone returns a deep copy so callers can snapshot and mutate independently.
func (d Deck) Clone() Deck {
	cp := make(Deck, len(d))
	copy(cp, d)
	return cp
}
