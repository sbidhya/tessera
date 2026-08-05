package engine

import (
	"math/rand/v2"
)

// GameConfig tunes a match. Zero values are replaced with defaults by
// NewGame so callers need only set what they care about.
type GameConfig struct {
	// SequencesToWin is how many distinct 5-in-a-rows a player needs to win.
	// Default 2; set to 1 for fast test games.
	SequencesToWin int
	// HandSize is cards dealt to each player at start. Default 7 for 2-player
	// Sequence (traditional). Tests often want smaller hands.
	HandSize int
}

// DefaultGameConfig is the standard 2-player configuration.
var DefaultGameConfig = GameConfig{SequencesToWin: 2, HandSize: 7}

func (c GameConfig) withDefaults() GameConfig {
	if c.SequencesToWin <= 0 {
		c.SequencesToWin = DefaultGameConfig.SequencesToWin
	}
	if c.HandSize <= 0 {
		c.HandSize = DefaultGameConfig.HandSize
	}
	return c
}

// Player holds per-player mutable state.
type Player struct {
	ID   string `json:"id"`
	Hand []Card `json:"hand"`
}

// Chip occupancy: -1 empty, 0..n-1 player index. Corners stay -1 but are
// treated as wild by FindSequences.
const chipEmpty = -1

// GameState is the fully authoritative snapshot of one match. It is
// immutable after construction — ApplyMove returns a new copy — so the pure
// engine stays free of data races and the room actor can own the current
// value without locks.
type GameState struct {
	Board       Board                   `json:"board"`
	Players     []Player                `json:"players"`
	CurrentTurn int                     `json:"current_turn"` // index into Players
	Deck        Deck                    `json:"deck"`
	DiscardPile []Card                  `json:"discard_pile"`
	Chips       [BoardSize][BoardSize]int  `json:"chips"`
	Locked      [BoardSize][BoardSize]bool `json:"locked"`
	Winner      *int                    `json:"winner,omitempty"`
	TurnCount   int                     `json:"turn_count"` // number of moves applied
	Config      GameConfig              `json:"config"`
	// Sequences counts packed sequences per player index for convenience.
	Sequences []int `json:"sequences"`
}

// Move is a player's intended action on their turn.
type Move struct {
	PlayerID string `json:"player_id"`
	Card     Card   `json:"card"`
	Row      int    `json:"row"`
	Col      int    `json:"col"`
	// IsDiscard indicates a dead-card discard (Row/Col ignored). The card
	// must be present in the player's hand and both matching board cells must
	// already be occupied.
	IsDiscard bool `json:"is_discard"`
}

// NewGame creates a new 2-player match in the canonical starting position.
// rng drives the initial deck shuffle (and optional random starting player)
// and must be non-nil — pass Config.NewRand for reproducibility. cfg zero
// values are defaulted.
func NewGame(playerIDs []string, rng *rand.Rand, cfg GameConfig) (*GameState, error) {
	if len(playerIDs) != 2 {
		return nil, ErrInvalidPlayerCount
	}
	seen := map[string]bool{}
	for _, id := range playerIDs {
		if id == "" {
			return nil, ErrEmptyPlayerID
		}
		if seen[id] {
			return nil, ErrDuplicatePlayerID
		}
		seen[id] = true
	}
	if rng == nil {
		panic("engine: NewGame requires non-nil rng")
	}
	cfg = cfg.withDefaults()

	board := NewBoard()
	deck := NewDeck(rng)

	players := make([]Player, len(playerIDs))
	for i, id := range playerIDs {
		players[i] = Player{ID: id}
	}

	// Deal round-robin so earlier players don't get a biased top-of-deck slice.
	for i := 0; i < cfg.HandSize; i++ {
		for p := range players {
			c, ok := deck.Draw()
			if !ok {
				// Not enough cards to deal — deck was too small (e.g. test with
				// tiny deck). Return what we have; caller can decide.
				break
			}
			players[p].Hand = append(players[p].Hand, c)
		}
	}

	var chips [BoardSize][BoardSize]int
	for r := 0; r < BoardSize; r++ {
		for c := 0; c < BoardSize; c++ {
			chips[r][c] = chipEmpty
		}
	}

	// Pick a random starting player deterministically from rng.
	start := 0
	if rng != nil {
		start = rng.IntN(len(players))
	}

	gs := &GameState{
		Board:       board,
		Players:     players,
		CurrentTurn: start,
		Deck:        deck,
		Chips:       chips,
		Config:      cfg,
		Sequences:   make([]int, len(players)),
	}
	// Initial locked is empty.
	return gs, nil
}

// Clone returns a deep copy of the game so ApplyMove can be pure.
func (g *GameState) Clone() *GameState {
	cp := *g
	// Deep-copy slices that share backing arrays.
	cp.Players = make([]Player, len(g.Players))
	for i, p := range g.Players {
		cp.Players[i].ID = p.ID
		cp.Players[i].Hand = append([]Card(nil), p.Hand...)
	}
	cp.Deck = g.Deck.Clone()
	cp.DiscardPile = append([]Card(nil), g.DiscardPile...)
	cp.Sequences = append([]int(nil), g.Sequences...)
	// Board, Chips, Locked, Config are value types / arrays and already copied.
	if g.Winner != nil {
		w := *g.Winner
		cp.Winner = &w
	}
	return &cp
}

// PlayerIndex returns the index of the player with the given id.
func (g *GameState) PlayerIndex(id string) (int, bool) {
	for i, p := range g.Players {
		if p.ID == id {
			return i, true
		}
	}
	return -1, false
}

// CurrentPlayerID is the id whose turn it is (empty if game already won
// and caller still inspects, but normally check Winner first).
func (g *GameState) CurrentPlayerID() string {
	if g.CurrentTurn < 0 || g.CurrentTurn >= len(g.Players) {
		return ""
	}
	return g.Players[g.CurrentTurn].ID
}

// IsTerminal reports whether the game has a winner.
func (g *GameState) IsTerminal() bool { return g.Winner != nil }

// containsCard reports whether hand contains card and returns its index.
func containsCard(hand []Card, card Card) (int, bool) {
	for i, c := range hand {
		if c == card {
			return i, true
		}
	}
	return -1, false
}

// ValidateMove checks whether m is legal in the current state without
// mutating anything. It mirrors the checks in ApplyMove and is exposed so
// callers (and tests) can distinguish validation failures from application.
func (g *GameState) ValidateMove(m Move) error {
	if g.IsTerminal() {
		return ErrGameOver
	}
	pIdx, ok := g.PlayerIndex(m.PlayerID)
	if !ok {
		return ErrPlayerNotFound
	}
	if pIdx != g.CurrentTurn {
		return ErrOutOfTurn
	}
	if _, ok := containsCard(g.Players[pIdx].Hand, m.Card); !ok {
		return ErrCardNotInHand
	}

	if m.IsDiscard {
		if IsJack(m.Card) {
			return ErrJackCannotBeDead
		}
		pos := g.Board.PositionsFor(m.Card)
		if len(pos) != 2 {
			// Should never happen for non-jack in our board, but treat as dead-check failure.
			return ErrNotDeadCard
		}
		// Both positions must be occupied by any chip.
		for _, p := range pos {
			if g.Chips[p.Row][p.Col] == chipEmpty {
				return ErrNotDeadCard
			}
		}
		return nil
	}

	// Non-discard moves need a target.
	if m.Row < 0 || m.Row >= BoardSize || m.Col < 0 || m.Col >= BoardSize {
		return ErrCellOutOfBounds
	}
	if IsCorner(m.Row, m.Col) {
		return ErrCornerNotPlayable
	}

	if IsOneEyedJack(m.Card) {
		occ := g.Chips[m.Row][m.Col]
		if occ == chipEmpty {
			return ErrCellEmpty
		}
		if occ == pIdx {
			return ErrCannotRemoveOwnChip
		}
		if g.Locked[m.Row][m.Col] {
			return ErrCannotRemoveLockedChip
		}
		return nil
	}
	if IsTwoEyedJack(m.Card) {
		if g.Chips[m.Row][m.Col] != chipEmpty {
			return ErrCellOccupied
		}
		return nil
	}
	// Normal card: must match board face and be empty.
	boardCard, ok := g.Board.CardAt(m.Row, m.Col)
	if !ok {
		return ErrCardDoesNotMatchCell
	}
	if boardCard != m.Card {
		return ErrCardDoesNotMatchCell
	}
	if g.Chips[m.Row][m.Col] != chipEmpty {
		return ErrCellOccupied
	}
	return nil
}

// ApplyMove validates m and, if legal, returns the next GameState after
// applying it, drawing a replacement card, recomputing sequences/locks, and
// advancing the turn (or marking a winner). The receiver is never mutated.
func (g *GameState) ApplyMove(m Move) (*GameState, error) {
	if err := g.ValidateMove(m); err != nil {
		return nil, err
	}
	pIdx, _ := g.PlayerIndex(m.PlayerID)
	next := g.Clone()

	// Helper to remove the played card from hand, push to discard, and draw.
	consumeCard := func() {
		hand := next.Players[pIdx].Hand
		cardIdx, _ := containsCard(hand, m.Card)
		// Remove by swapping with last then truncating — order in hand does not
		// matter for correctness and tests compare unordered or search linearly.
		// Preserve order for determinism: splice.
		next.Players[pIdx].Hand = append(hand[:cardIdx], hand[cardIdx+1:]...)
		next.DiscardPile = append(next.DiscardPile, m.Card)
		if c, ok := next.Deck.Draw(); ok {
			next.Players[pIdx].Hand = append(next.Players[pIdx].Hand, c)
		}
	}

	if m.IsDiscard {
		consumeCard()
		next.TurnCount++
		// No placement, so no new sequences. Advance turn if still running.
		if next.Winner == nil {
			next.CurrentTurn = (next.CurrentTurn + 1) % len(next.Players)
		}
		return next, nil
	}

	if IsOneEyedJack(m.Card) {
		next.Chips[m.Row][m.Col] = chipEmpty
	} else {
		// Two-eyed jack or normal placement.
		next.Chips[m.Row][m.Col] = pIdx
	}
	consumeCard()

	// Recompute packed sequences and locks for all players.
	all := AllSequences(next.Chips, len(next.Players))
	next.Locked = LockedGrid(next.Chips, len(next.Players))
	for i, seqs := range all {
		next.Sequences[i] = len(seqs)
	}

	// Check win: first player to reach SequencesToWin wins. If both somehow
	// reach simultaneously (e.g. removal undoes something — not possible with
	// our locked rule, but be deterministic), the player who just moved wins
	// if they are among winners; otherwise lowest index wins.
	var winner *int
	for i, n := range next.Sequences {
		if n >= next.Config.SequencesToWin {
			// Prefer the mover if they are a winner.
			if i == pIdx {
				w := i
				winner = &w
				break
			}
			if winner == nil {
				w := i
				winner = &w
			}
		}
	}
	if winner != nil {
		next.Winner = winner
	} else {
		next.TurnCount++
		next.CurrentTurn = (next.CurrentTurn + 1) % len(next.Players)
	}
	return next, nil
}
