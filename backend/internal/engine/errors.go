package engine

import "errors"

// Sentinel errors returned by move validation. They are exported so upper layers
// (the room manager, transport) can distinguish "your move was illegal" from a
// transport/system failure and surface a precise reason to the client.
var (
	// ErrGameOver is returned when a move is attempted after a winner exists.
	ErrGameOver = errors.New("engine: game is over")
	// ErrNotYourTurn is returned when a player moves out of turn.
	ErrNotYourTurn = errors.New("engine: not this player's turn")
	// ErrUnknownPlayer is returned for a player id not in the game.
	ErrUnknownPlayer = errors.New("engine: unknown player")
	// ErrCardNotInHand is returned when the player does not hold the played card.
	ErrCardNotInHand = errors.New("engine: card not in hand")
	// ErrCellOutOfBounds is returned for a target cell off the board.
	ErrCellOutOfBounds = errors.New("engine: cell out of bounds")
	// ErrCellOccupied is returned when placing on a cell that already has a chip.
	ErrCellOccupied = errors.New("engine: cell already occupied")
	// ErrCellIsCorner is returned when targeting a wild corner (no chip may be
	// placed on or removed from a corner).
	ErrCellIsCorner = errors.New("engine: cell is a wild corner")
	// ErrCardCellMismatch is returned when a non-jack card is played onto a cell
	// that does not bear that card.
	ErrCardCellMismatch = errors.New("engine: card does not match target cell")
	// ErrNotRemovable is returned when a one-eyed jack targets a cell that has no
	// removable opponent chip (empty, own chip, or locked in a sequence).
	ErrNotRemovable = errors.New("engine: no removable opponent chip on cell")
	// ErrNotOneEyedJack is returned when a remove move is made without a one-eyed
	// jack.
	ErrNotOneEyedJack = errors.New("engine: remove requires a one-eyed jack")
	// ErrCardNotDead is returned when a dead-card swap targets a card that still
	// has at least one open board cell.
	ErrCardNotDead = errors.New("engine: card is not dead")
	// ErrDeadCardUsed is returned when a second dead-card swap is attempted in the
	// same turn.
	ErrDeadCardUsed = errors.New("engine: dead-card swap already used this turn")
	// ErrJackNotPlaceable is returned when a one-eyed jack is used to place a chip
	// (one-eyed jacks only remove).
	ErrJackNotPlaceable = errors.New("engine: one-eyed jack cannot place a chip")
)
