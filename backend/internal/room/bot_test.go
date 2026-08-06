package room

import "github.com/sbidhya/tessera/backend/internal/engine"

// A deliberately simple, fully deterministic bot used to drive complete games
// through the room in tests. It sees only what a real client sees — a Snapshot —
// so if it can play a game, so can the transport layer in B3.
//
// The bot has no strategy beyond "get to five in a row fastest", which is
// exactly what the tests need: games that terminate in a bounded number of
// moves without any randomness of their own.

// botMove is the engine-level part of a move choice; the test wraps it with a
// MoveID before submitting.
type botMove struct {
	Type engine.MoveType
	Card engine.Card
	Cell engine.Cell
}

// botDirections mirrors the engine's four line orientations.
var botDirections = [4]engine.Cell{
	{Row: 0, Col: 1},
	{Row: 1, Col: 0},
	{Row: 1, Col: 1},
	{Row: 1, Col: -1},
}

// countsFor reports whether a cell already works for player p: their chip, or a
// wild corner.
func countsFor(s Snapshot, c engine.Cell, p engine.PlayerID) bool {
	if !c.InBounds() {
		return false
	}
	if s.Board.IsCorner(c) {
		return true
	}
	ch, ok := s.Chips[c]
	return ok && ch.Owner == p
}

// isOpen reports whether a chip may be placed on a cell.
func isOpen(s Snapshot, c engine.Cell) bool {
	if !c.InBounds() || s.Board.IsCorner(c) {
		return false
	}
	_, occupied := s.Chips[c]
	return !occupied
}

// placementScore is the length of the longest run through target that would
// count for p if p placed there. Higher is closer to a sequence, so maximizing
// it drives games to a win quickly and keeps test runtimes bounded.
func placementScore(s Snapshot, target engine.Cell, p engine.PlayerID) int {
	best := 1
	for _, d := range botDirections {
		n := 1
		for k := 1; k < 5; k++ {
			c := engine.Cell{Row: target.Row + k*d.Row, Col: target.Col + k*d.Col}
			if !countsFor(s, c, p) {
				break
			}
			n++
		}
		for k := 1; k < 5; k++ {
			c := engine.Cell{Row: target.Row - k*d.Row, Col: target.Col - k*d.Col}
			if !countsFor(s, c, p) {
				break
			}
			n++
		}
		if n > best {
			best = n
		}
	}
	return best
}

// allCells lists every board cell in row-major order, so scans are deterministic.
func allCells() []engine.Cell {
	cells := make([]engine.Cell, 0, engine.BoardSize*engine.BoardSize)
	for row := 0; row < engine.BoardSize; row++ {
		for col := 0; col < engine.BoardSize; col++ {
			cells = append(cells, engine.Cell{Row: row, Col: col})
		}
	}
	return cells
}

// chooseMove picks the best available placement for the seat, falling back to a
// one-eyed jack removal when nothing can be placed. ok is false when the seat
// has no legal move at all (the caller should then try a dead-card swap).
func chooseMove(s Snapshot, seat engine.PlayerID) (botMove, bool) {
	best, bestScore := botMove{}, -1

	for _, card := range s.Hands[seat] {
		var targets []engine.Cell
		switch {
		case card.IsOneEyedJack():
			continue // removal only; handled below
		case card.IsTwoEyedJack():
			for _, c := range allCells() {
				if isOpen(s, c) {
					targets = append(targets, c)
				}
			}
		default:
			for _, c := range s.Board.CellsFor(card) {
				if isOpen(s, c) {
					targets = append(targets, c)
				}
			}
		}
		for _, c := range targets {
			score := placementScore(s, c, seat)
			if card.IsTwoEyedJack() {
				// A wild jack can go anywhere, so it would otherwise always win
				// the tie-break. Discount it so it is spent on a real advantage.
				score--
			}
			if score > bestScore {
				best, bestScore = botMove{Type: engine.MovePlace, Card: card, Cell: c}, score
			}
		}
	}
	if bestScore >= 0 {
		return best, true
	}

	// Nothing placeable: burn a one-eyed jack on the first removable chip.
	for _, card := range s.Hands[seat] {
		if !card.IsOneEyedJack() {
			continue
		}
		for _, c := range allCells() {
			if ch, ok := s.Chips[c]; ok && ch.Owner != seat && !ch.InSequence {
				return botMove{Type: engine.MoveRemove, Card: card, Cell: c}, true
			}
		}
	}
	return botMove{}, false
}

// chooseDeadCard returns a dead card in the seat's hand: a non-jack whose two
// board cells are both taken, so it can never be played.
func chooseDeadCard(s Snapshot, seat engine.PlayerID) (engine.Card, bool) {
	for _, card := range s.Hands[seat] {
		if card.IsJack() {
			continue
		}
		cells := s.Board.CellsFor(card)
		if len(cells) == 0 {
			continue
		}
		dead := true
		for _, c := range cells {
			if isOpen(s, c) {
				dead = false
				break
			}
		}
		if dead {
			return card, true
		}
	}
	return engine.Card{}, false
}
