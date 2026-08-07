package room

import "errors"

// Sentinel errors returned by room and manager operations. They are separate
// from the engine's rule errors (engine.ErrNotYourTurn, ...): these describe
// membership, lifecycle, and delivery problems, whereas the engine's describe
// illegal *game* moves. Upper layers (B3 transport) map both onto client-facing
// error codes, so keeping the two vocabularies distinct matters.
var (
	// ErrRoomClosed is returned when a command is submitted to a room whose
	// goroutine has stopped (Room.Close or Manager.Shutdown).
	ErrRoomClosed = errors.New("room: room is closed")
	// ErrRoomFull is returned when a new player tries to join a room with no
	// free seat. Rejoining an existing seat is always allowed.
	ErrRoomFull = errors.New("room: no free seat")
	// ErrNotSeated is returned when a player who never joined (or has left a
	// not-yet-started room) submits a move or leaves.
	ErrNotSeated = errors.New("room: player is not seated in this room")
	// ErrGameNotStarted is returned for moves submitted before every seat is
	// filled.
	ErrGameNotStarted = errors.New("room: game has not started")
	// ErrGameFinished is returned when joining a room whose match is over.
	ErrGameFinished = errors.New("room: game is finished")
	// ErrInvalidPlayerID is returned for an empty player id.
	ErrInvalidPlayerID = errors.New("room: player id must not be empty")
	// ErrMissingMoveID is returned for a move with no move id. The id is what
	// makes retries idempotent, so it is mandatory rather than optional.
	ErrMissingMoveID = errors.New("room: move id must not be empty")
	// ErrStaleSeq is returned when a move's ExpectedSeq does not match the
	// room's current sequence number: the client acted on a state it has since
	// stopped seeing. Wrapped with the two values for diagnosis.
	ErrStaleSeq = errors.New("room: stale sequence number")
	// ErrNoSuchRoom is returned by Manager for an unknown room id.
	ErrNoSuchRoom = errors.New("room: no such room")
	// ErrManagerClosed is returned by Manager.Create after Shutdown.
	ErrManagerClosed = errors.New("room: manager is shut down")
)
