package engine

import "errors"

// Sentinel errors for move validation. Callers should use errors.Is to test
// for a specific failure so message wording can evolve without breaking them.
var (
	ErrGameOver               = errors.New("game already finished")
	ErrPlayerNotFound         = errors.New("player not found")
	ErrOutOfTurn              = errors.New("not player's turn")
	ErrCardNotInHand          = errors.New("card not in hand")
	ErrCellOutOfBounds        = errors.New("cell out of bounds")
	ErrCornerNotPlayable      = errors.New("corner is wild and cannot be played onto")
	ErrCellOccupied           = errors.New("cell already occupied")
	ErrCellEmpty              = errors.New("cell is empty")
	ErrCannotRemoveOwnChip    = errors.New("cannot remove own chip")
	ErrCannotRemoveLockedChip = errors.New("cannot remove chip that is part of a completed sequence")
	ErrCardDoesNotMatchCell   = errors.New("card does not match board cell")
	ErrNotDeadCard            = errors.New("card is not dead (at least one matching cell is still open)")
	ErrJackCannotBeDead       = errors.New("jack cannot be a dead card")
	ErrInvalidPlayerCount     = errors.New("game requires exactly 2 players")
	ErrDuplicatePlayerID      = errors.New("duplicate player id")
	ErrEmptyPlayerID          = errors.New("player id must not be empty")
)
