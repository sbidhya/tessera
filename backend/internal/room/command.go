package room

import "context"

// This file is the room's mailbox plumbing: how a caller on any goroutine asks
// the room's single owning goroutine to do something and gets an answer back.
//
// The shape is deliberately uniform — one command type per operation, each
// carrying its arguments and a private reply channel. Keeping the transfer
// explicit (rather than, say, shipping arbitrary closures into the room) means
// the set of things that can happen to a match is a closed, readable list, and
// B4 can log exactly that list to the WAL.

// command is one message in a room's mailbox. execute runs ON the room
// goroutine and is therefore allowed to touch the room's owned state.
type command interface {
	execute(r *Room)
}

// result carries a command's reply: a typed value plus a rejection reason.
type result[T any] struct {
	val T
	err error
}

// call submits the command built by mk and waits for its reply.
//
// Three things can happen while waiting, and all three must terminate:
//   - the room replies (the normal path);
//   - the room shuts down, closing done, so we report ErrRoomClosed instead of
//     blocking forever on a reply that will never come;
//   - the caller's context is cancelled (a client disconnects mid-request), so
//     we return without waiting on the room at all.
func call[T any](ctx context.Context, r *Room, mk func(reply chan<- result[T]) command) (T, error) {
	var zero T

	// Buffered with room for exactly one reply. This is what lets the room
	// goroutine hand back a result and move on even if the caller has already
	// walked away on context cancellation — an unbuffered channel would wedge
	// the whole match behind one abandoned request.
	reply := make(chan result[T], 1)

	select {
	case r.cmds <- mk(reply):
	case <-r.done:
		return zero, ErrRoomClosed
	case <-ctx.Done():
		return zero, ctx.Err()
	}

	select {
	case res := <-reply:
		return res.val, res.err
	case <-r.done:
		return zero, ErrRoomClosed
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

type joinCmd struct {
	playerID string
	reply    chan<- result[JoinResult]
}

func (c joinCmd) execute(r *Room) {
	val, err := r.join(c.playerID)
	c.reply <- result[JoinResult]{val: val, err: err}
}

type moveCmd struct {
	req   MoveRequest
	reply chan<- result[MoveResult]
}

func (c moveCmd) execute(r *Room) {
	val, err := r.playMove(c.req)
	c.reply <- result[MoveResult]{val: val, err: err}
}

type leaveCmd struct {
	playerID string
	reply    chan<- result[struct{}]
}

func (c leaveCmd) execute(r *Room) {
	c.reply <- result[struct{}]{err: r.leave(c.playerID)}
}

type snapshotCmd struct {
	viewer string
	reply  chan<- result[Snapshot]
}

func (c snapshotCmd) execute(r *Room) {
	c.reply <- result[Snapshot]{val: r.snapshot(c.viewer)}
}
