package room

import "errors"

// Sentinel errors returned by the room manager. Engine rule violations are NOT
// wrapped — GameState.Apply's sentinels (engine.ErrNotYourTurn, …) pass through
// unchanged, so transport can distinguish "you broke a rule" from "the room
// refused to look at your command" with a single errors.Is chain.
var (
	// ErrRoomClosed is returned when a command is submitted to a room whose
	// goroutine has stopped.
	ErrRoomClosed = errors.New("room: room is closed")
	// ErrRoomFull is returned when a new player tries to join a room with no
	// free seats.
	ErrRoomFull = errors.New("room: no free seats")
	// ErrGameNotStarted is returned for a move submitted before every seat is
	// filled.
	ErrGameNotStarted = errors.New("room: game has not started")
	// ErrNotInRoom is returned when a player who holds no seat submits a move or
	// leaves.
	ErrNotInRoom = errors.New("room: player is not in this room")
	// ErrMissingMoveID is returned for a move with an empty MoveID. The id is
	// mandatory: it is what makes retries idempotent.
	ErrMissingMoveID = errors.New("room: move_id is required")
	// ErrStaleSeq is returned when a move's ExpectedSeq does not match the room's
	// current game version — the client acted on a stale view of the board and
	// must resync (fetch a snapshot) before retrying.
	ErrStaleSeq = errors.New("room: stale expected_seq")
	// ErrUnknownPlayer is returned for an empty player id.
	ErrUnknownPlayer = errors.New("room: empty player id")
)

// Manager-level errors.
var (
	// ErrNoSuchRoom is returned by Manager.Get/Remove for an unknown room id.
	ErrNoSuchRoom = errors.New("room: no such room")
	// ErrManagerClosed is returned when creating a room on a shut-down manager.
	ErrManagerClosed = errors.New("room: manager is closed")
)
