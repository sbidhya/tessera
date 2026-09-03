package engine

// MoveType distinguishes the kinds of action a player can take on their turn.
type MoveType uint8

const (
	// MovePlace plays a card to place a chip: a normal card on its matching open
	// cell, or a two-eyed jack on any open cell.
	MovePlace MoveType = iota
	// MoveRemove plays a one-eyed jack to remove an opponent's unlocked chip.
	MoveRemove
	// MoveDeadCard discards a dead card (both its board cells occupied) and draws
	// a replacement. Allowed once per turn and does NOT end the turn.
	MoveDeadCard
)

// Move is a single action submitted by a player. Cell is unused for MoveDeadCard.
type Move struct {
	Player PlayerID
	Type   MoveType
	Card   Card
	Cell   Cell
}

// Apply validates and applies a move to the game state. On any error it returns
// the state unchanged (validation happens fully before mutation), giving the
// room manager transactional semantics for idempotent, out-of-turn-rejecting
// command handling.
//
// MovePlace and MoveRemove consume the played card, draw a replacement, run
// sequence detection through the affected cell, may set a winner, and advance the
// turn. MoveDeadCard swaps one dead card and leaves the turn with the same player.
//
// A successful move that leaves the draw pile empty and the player to move with
// no legal move marks the game Drawn (see GameState.Drawn). A win takes
// precedence: a winning move never becomes a draw even if it also empties the
// pile.
func (gs *GameState) Apply(m Move) error {
	if gs.GameOver() {
		return ErrGameOver
	}
	if !gs.knownPlayer(m.Player) {
		return ErrUnknownPlayer
	}
	if m.Player != gs.Turn {
		return ErrNotYourTurn
	}
	idx := gs.handIndex(m.Player, m.Card)
	if idx < 0 {
		return ErrCardNotInHand
	}

	switch m.Type {
	case MovePlace:
		return gs.applyPlace(m, idx)
	case MoveRemove:
		return gs.applyRemove(m, idx)
	case MoveDeadCard:
		return gs.applyDeadCard(m, idx)
	default:
		return ErrCardCellMismatch
	}
}

// applyPlace handles placing a chip with a normal card or a two-eyed jack.
func (gs *GameState) applyPlace(m Move, handIdx int) error {
	if !m.Cell.InBounds() {
		return ErrCellOutOfBounds
	}
	if gs.Board.IsCorner(m.Cell) {
		return ErrCellIsCorner
	}
	if _, occupied := gs.Chips[m.Cell]; occupied {
		return ErrCellOccupied
	}

	switch {
	case m.Card.IsOneEyedJack():
		// One-eyed jacks remove; they cannot place.
		return ErrJackNotPlaceable
	case m.Card.IsTwoEyedJack():
		// Wild: any open non-corner cell is fine — no card/cell match required.
	default:
		// Normal card: the target cell must bear this exact card.
		boardCard, ok := gs.Board.CardAt(m.Cell)
		if !ok || boardCard != m.Card {
			return ErrCardCellMismatch
		}
	}

	// Validation complete — mutate.
	gs.Chips[m.Cell] = Chip{Owner: m.Player}
	gs.spendAndDraw(m.Player, handIdx)
	gs.scoreAndAdvance(m.Cell, m.Player)
	return nil
}

// applyRemove handles a one-eyed jack removing an opponent's unlocked chip.
func (gs *GameState) applyRemove(m Move, handIdx int) error {
	if !m.Card.IsOneEyedJack() {
		return ErrNotOneEyedJack
	}
	if !m.Cell.InBounds() {
		return ErrCellOutOfBounds
	}
	if gs.Board.IsCorner(m.Cell) {
		return ErrCellIsCorner
	}
	chip, ok := gs.Chips[m.Cell]
	if !ok || chip.Owner == m.Player || chip.InSequence {
		// Nothing to remove, own chip, or locked into a completed sequence.
		return ErrNotRemovable
	}

	// Validation complete — mutate. Removing a chip cannot complete a sequence,
	// so no detection runs, but the turn still ends.
	delete(gs.Chips, m.Cell)
	gs.spendAndDraw(m.Player, handIdx)
	gs.nextTurn()
	gs.checkDraw()
	return nil
}

// applyDeadCard handles discarding a dead card and drawing a replacement.
func (gs *GameState) applyDeadCard(m Move, handIdx int) error {
	if gs.deadCardUsed {
		return ErrDeadCardUsed
	}
	if !gs.isDead(m.Card) {
		return ErrCardNotDead
	}

	// Validation complete — mutate. The turn does NOT advance: the player still
	// makes a normal play afterwards.
	gs.Discard = append(gs.Discard, m.Card)
	gs.removeFromHand(m.Player, handIdx)
	if drawn, ok := gs.drawCard(); ok {
		gs.Hands[m.Player] = append(gs.Hands[m.Player], drawn)
	}
	gs.deadCardUsed = true
	gs.checkDraw()
	return nil
}

// isDead reports whether a held card is dead: it is a non-jack card whose two
// board cells are both occupied, so it can never be played. Jacks are never dead
// (they are always playable as wild/remove actions).
func (gs *GameState) isDead(card Card) bool {
	if card.IsJack() {
		return false
	}
	cells := gs.Board.CellsFor(card)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if _, occupied := gs.Chips[c]; !occupied {
			return false
		}
	}
	return true
}

// spendAndDraw discards the played card and draws a replacement into the player's
// hand (if the draw pile is non-empty).
func (gs *GameState) spendAndDraw(p PlayerID, handIdx int) {
	spent := gs.Hands[p][handIdx]
	gs.Discard = append(gs.Discard, spent)
	gs.removeFromHand(p, handIdx)
	if drawn, ok := gs.drawCard(); ok {
		gs.Hands[p] = append(gs.Hands[p], drawn)
	}
}

// scoreAndAdvance runs sequence detection through the just-placed cell, updates
// the winner if the player has reached SequencesToWin, and advances the turn if
// the game is not over. When the game continues with an empty draw pile, it
// marks a draw if the player to move has no legal move.
func (gs *GameState) scoreAndAdvance(placed Cell, p PlayerID) {
	if n := gs.detectSequencesThrough(placed, p); n > 0 {
		gs.SequencesWon[p] += n
		if gs.SequencesWon[p] >= gs.SequencesToWin {
			gs.Winner = p
			return // game over; do not advance the turn
		}
	}
	gs.nextTurn()
	gs.checkDraw()
}

// checkDraw marks the game drawn when the draw pile is exhausted and the player
// to move has no legal move. A decided game stays decided: it never overrides
// a winner and never clears Drawn.
func (gs *GameState) checkDraw() {
	if gs.Winner != NoPlayer || gs.Drawn {
		return
	}
	if len(gs.Draw) != 0 {
		return
	}
	if !gs.HasLegalMove(gs.Turn) {
		gs.Drawn = true
	}
}

// HasLegalMove reports whether player p can make any legal move from the
// current position, given the current per-turn dead-card allowance. Normal
// cards are always actionable (either placed on an open matching cell or, when
// dead, swapped); two-eyed jacks need any open cell; one-eyed jacks need a
// removable opponent chip. An empty hand has no legal move.
func (gs *GameState) HasLegalMove(p PlayerID) bool {
	hand := gs.Hands[p]
	if len(hand) == 0 {
		return false
	}
	openComputed, open := false, false
	removableComputed, removable := false, false
	for _, card := range hand {
		switch {
		case card.IsTwoEyedJack():
			if !openComputed {
				open = gs.hasOpenCell()
				openComputed = true
			}
			if open {
				return true
			}
		case card.IsOneEyedJack():
			if !removableComputed {
				removable = gs.hasRemovableChip(p)
				removableComputed = true
			}
			if removable {
				return true
			}
		default:
			for _, cell := range gs.Board.CellsFor(card) {
				if _, occupied := gs.Chips[cell]; !occupied && !gs.Board.IsCorner(cell) {
					return true
				}
			}
			if !gs.deadCardUsed && gs.isDead(card) {
				return true
			}
		}
	}
	return false
}

// hasOpenCell reports whether any non-corner cell is free of chips.
func (gs *GameState) hasOpenCell() bool {
	for row := 0; row < BoardSize; row++ {
		for col := 0; col < BoardSize; col++ {
			cell := Cell{Row: row, Col: col}
			if gs.Board.IsCorner(cell) {
				continue
			}
			if _, occupied := gs.Chips[cell]; !occupied {
				return true
			}
		}
	}
	return false
}

// hasRemovableChip reports whether p can remove any chip: an opponent's chip
// not locked into a completed sequence.
func (gs *GameState) hasRemovableChip(p PlayerID) bool {
	for _, chip := range gs.Chips {
		if chip.Owner != p && !chip.InSequence {
			return true
		}
	}
	return false
}
