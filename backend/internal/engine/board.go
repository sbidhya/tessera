package engine

// BoardSize is the fixed Sequence board dimension (10×10 = 100 cells).
const BoardSize = 10

// Position is a board coordinate (Row, Col) with 0-based indices.
type Position struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// Cell is one board square. Non-corner cells hold exactly one card face;
// corners are wild (Card == nil, IsCorner == true) and belong to every
// player for sequence detection but can never be played onto.
type Cell struct {
	Pos      Position `json:"pos"`
	Card     *Card    `json:"card,omitempty"`
	IsCorner bool     `json:"is_corner"`
}

// Board is the immutable 10×10 Sequence layout. Each non-jack card appears
// exactly twice; the four corners are wild. The layout is deterministic and
// fixed for all games so tests and clients can reason about card → cell
// mappings without needing a seed for the board itself. Deck shuffles still
// provide per-game randomness.
type Board [BoardSize][BoardSize]Cell

// IsCorner reports whether (row, col) is one of the four wild corners.
func IsCorner(row, col int) bool {
	return (row == 0 && col == 0) ||
		(row == 0 && col == BoardSize-1) ||
		(row == BoardSize-1 && col == 0) ||
		(row == BoardSize-1 && col == BoardSize-1)
}

// NewBoard returns the canonical Tessera board. The card → cell mapping is
// documented so tests can rely on it: after marking the four corners as
// wild, the remaining 96 cells are filled row-major (top-left to
// bottom-right, skipping corners) with a deterministic ordering of the 48
// non-jack cards duplicated in sorted order (Hearts → Diamonds → Clubs →
// Spades, Ace → King skipping Jack). So position (0,1) is Ace♥, (0,2) is 2♥,
// etc. The board never changes after creation.
func NewBoard() Board {
	var b Board
	// Initialise cell coordinates and corner flag.
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			b[r][c] = Cell{
				Pos:      Position{Row: r, Col: c},
				IsCorner: IsCorner(r, c),
			}
		}
	}

	// Build the 48 distinct non-jack cards in deterministic sorted order.
	var singles []Card
	for s := SuitHearts; s <= SuitSpades; s++ {
		for r := RankAce; r <= RankKing; r++ {
			if r == RankJack {
				continue
			}
			singles = append(singles, Card{Suit: s, Rank: r})
		}
	}
	// Duplicate to 96 and assign row-major.
	var all []Card
	all = append(all, singles...)
	all = append(all, singles...)
	idx := 0
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			if b[r][c].IsCorner {
				continue
			}
			card := all[idx]
			b[r][c].Card = &card
			idx++
		}
	}
	return b
}

// CardAt returns the printed card face for (row, col). ok is false for
// out-of-bounds or for corners (which have no card).
func (b *Board) CardAt(row, col int) (Card, bool) {
	if row < 0 || row >= BoardSize || col < 0 || col >= BoardSize {
		return Card{}, false
	}
	cell := b[row][col]
	if cell.IsCorner || cell.Card == nil {
		return Card{}, false
	}
	return *cell.Card, true
}

// PositionsFor returns the board positions where the given card appears.
// Non-jack cards appear exactly twice, jacks and unknown cards zero times.
// Returned positions are sorted row-major.
func (b *Board) PositionsFor(card Card) []Position {
	if IsJack(card) {
		return nil
	}
	var out []Position
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			cell := b[r][c]
			if cell.IsCorner || cell.Card == nil {
				continue
			}
			if *cell.Card == card {
				out = append(out, Position{Row: r, Col: c})
			}
		}
	}
	return out
}

// At returns the Cell for (row, col). The second argument is false for
// out-of-bounds coordinates.
func (b *Board) At(row, col int) (Cell, bool) {
	if row < 0 || row >= BoardSize || col < 0 || col >= BoardSize {
		return Cell{}, false
	}
	return b[row][col], true
}
