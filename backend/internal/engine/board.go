package engine

import (
	"fmt"
	"math/rand/v2"
)

// BoardSize is the side length of the square Sequence board.
const BoardSize = 10

// Cell is a board coordinate. Row and Col are each in [0, BoardSize).
type Cell struct {
	Row, Col int
}

// String renders a cell as "(row,col)" for logs and test messages.
func (c Cell) String() string { return fmt.Sprintf("(%d,%d)", c.Row, c.Col) }

// InBounds reports whether the cell lies on the board.
func (c Cell) InBounds() bool {
	return c.Row >= 0 && c.Row < BoardSize && c.Col >= 0 && c.Col < BoardSize
}

// corners are the four wild "free" spaces. They belong to every player at once
// and never hold a chip; they always count toward a sequence for whoever is
// building one through them.
var corners = [4]Cell{
	{0, 0}, {0, BoardSize - 1}, {BoardSize - 1, 0}, {BoardSize - 1, BoardSize - 1},
}

// Board is the 10×10 playing surface: a fixed mapping from each non-corner cell
// to the card printed on it, plus the reverse index (card → its two cells).
//
// Design note: the board layout is DATA, generated once from an injected RNG.
// The engine's rules depend only on this mapping and the corner geometry, not on
// how the layout was produced. A canonical/fixed physical layout could be
// swapped in later by replacing NewBoard with zero changes to the rules — see
// project.prompt. We generate deterministically so the layout is correct by
// construction (every non-jack card appears exactly twice) and reproducible from
// the seed, rather than risking a hand-transcribed 100-cell table in the
// crown-jewel engine.
type Board struct {
	cards     [BoardSize][BoardSize]Card
	isCorner  [BoardSize][BoardSize]bool
	cardCells map[Card][]Cell
}

// NewBoard builds a valid board from the injected RNG. The 48 non-jack cards are
// each placed on exactly two of the 96 non-corner cells; the four corners are
// wild. Jacks never appear on the board (they are action cards).
//
// Determinism: the layout is a pure function of the RNG's stream, so the same
// seed always yields the same board.
func NewBoard(rng *rand.Rand) *Board {
	b := &Board{cardCells: make(map[Card][]Cell)}
	for _, c := range corners {
		b.isCorner[c.Row][c.Col] = true
	}

	// The pool: every non-jack card, twice — 48 × 2 = 96, matching the 96
	// non-corner cells exactly.
	pool := make([]Card, 0, 96)
	for _, s := range allSuits {
		for _, r := range allRanks {
			if r == Jack {
				continue
			}
			pool = append(pool, Card{Rank: r, Suit: s}, Card{Rank: r, Suit: s})
		}
	}
	Shuffle(rng, pool)

	// Fill non-corner cells in row-major order from the shuffled pool.
	i := 0
	for row := 0; row < BoardSize; row++ {
		for col := 0; col < BoardSize; col++ {
			if b.isCorner[row][col] {
				continue
			}
			card := pool[i]
			i++
			b.cards[row][col] = card
			cell := Cell{Row: row, Col: col}
			b.cardCells[card] = append(b.cardCells[card], cell)
		}
	}
	return b
}

// IsCorner reports whether the cell is one of the four wild free spaces.
func (b *Board) IsCorner(c Cell) bool {
	return c.InBounds() && b.isCorner[c.Row][c.Col]
}

// CardAt returns the card printed on a cell. ok is false for corners (which have
// no card) and for out-of-bounds cells.
func (b *Board) CardAt(c Cell) (card Card, ok bool) {
	if !c.InBounds() || b.isCorner[c.Row][c.Col] {
		return Card{}, false
	}
	return b.cards[c.Row][c.Col], true
}

// CellsFor returns the (up to two) cells that bear the given card. Jacks return
// an empty slice — they are not on the board. The returned slice is the board's
// own; callers must not mutate it.
func (b *Board) CellsFor(card Card) []Cell {
	return b.cardCells[card]
}
