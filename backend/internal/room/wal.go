package room

import "github.com/sbidhya/tessera/backend/internal/engine"

// WAL is the durability hook the room calls before mutating state.
// It is defined here (the inner layer) and implemented by the outer
// persistence layer (internal/wal), so the room never imports persistence.
type WAL interface {
	LogCreate(roomID string, opts engine.Options) error
	LogJoin(roomID string, playerID string) error
	LogLeave(roomID string, playerID string) error
	LogMove(roomID string, req MoveRequest) error
}
