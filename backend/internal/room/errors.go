package room

import "errors"

// Sentinels returned by the room manager. They are exported so the transport
// layer (B3) can map them to HTTP/WS status codes and the tests can assert on
// precise failures with errors.Is.
var (
	// ErrRoomFull is returned when a Join would exceed the room's capacity.
	ErrRoomFull = errors.New("room: full")

	// ErrNotInRoom is returned when a token that has not joined tries to play
	// or leave.
	ErrNotInRoom = errors.New("room: not in room")

	// ErrRoomClosed is returned when an operation is attempted on a closed or
	// deleted room.
	ErrRoomClosed = errors.New("room: closed")

	// ErrStaleSequence is returned when the client's expected sequence number
	// does not match the server's current sequence. The client should refresh
	// state and retry.
	ErrStaleSequence = errors.New("room: stale sequence")

	// ErrInvalidMoveID is returned when a PlayMove is sent with an empty
	// move_id. B2's idempotency relies on the client supplying a stable id per
	// attempt.
	ErrInvalidMoveID = errors.New("room: invalid move_id")

	// ErrPlayerMismatch is returned when the Move.Player field does not match
	// the seat assigned to the caller's token.
	ErrPlayerMismatch = errors.New("room: move player does not match joined seat")
)
