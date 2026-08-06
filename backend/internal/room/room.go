// Package room holds the live, in-memory state of matches.
//
// Layering (see project.prompt): room sits directly outside the engine. It may
// import engine and config; it must not import transport or persistence.
//
// # The actor model
//
// Each room owns exactly one match's *engine.GameState, and that state is
// touched by exactly ONE goroutine — the room's run loop. Callers never reach
// into the state; they send a command down a channel and wait for a reply. The
// game state therefore needs no mutex at all: mutual exclusion comes from the
// fact that only one goroutine can be inside the run loop at a time.
//
// Why not just put a sync.Mutex on GameState? Two reasons:
//
//   - Correctness by construction. A mutex protects whatever the programmer
//     remembers to lock. Channel ownership makes it structurally impossible to
//     read or write the state from the wrong goroutine, because no pointer to it
//     ever escapes. Snapshot deep-copies precisely so nothing escapes.
//   - It gives us a serialization point. Every accepted command passes through
//     one ordered queue, which is exactly the hook the write-ahead log (B4)
//     needs: append to the log, then apply, then ack — in a single, already
//     serialized place.
//
// The cost is that a room processes commands strictly one at a time. That is
// fine: a room is one 2-player match, and applying a move is microseconds of
// pure computation. Rooms are independent, so throughput scales with room count.
//
// # Idempotency
//
// Networks deliver at-least-once. A client that retries a move after a timeout
// must not place two chips. Every move therefore carries a client-generated
// MoveID; the room remembers the ids it has accepted and replays the original
// acknowledgement for a duplicate instead of applying it again. Move ids are
// scoped per player, so two clients using simple counters can never collide.
//
// Moves also carry an optional ExpectedSeq — the game version the client
// believes it is acting on. A mismatch means the client's view is stale (it
// missed an update) and the move is rejected rather than applied to a board the
// player never saw. "Expected turn" needs no separate field: the engine already
// rejects a move whose player is not the player to move.
package room

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// PlayerID is a client's stable external identity — an opaque string minted by
// the client (B6 will issue tokens). It is deliberately distinct from
// engine.PlayerID, which is a seat index within one match: the same person may
// sit in seat 0 of one room and seat 1 of another.
type PlayerID string

// Status is the lifecycle stage of a room.
type Status uint8

const (
	// StatusWaiting means seats are still open; moves are rejected.
	StatusWaiting Status = iota
	// StatusPlaying means every seat is filled and moves are accepted.
	StatusPlaying
	// StatusFinished means the match has a winner.
	StatusFinished
)

// String renders a status for logs and JSON.
func (s Status) String() string {
	switch s {
	case StatusWaiting:
		return "waiting"
	case StatusPlaying:
		return "playing"
	case StatusFinished:
		return "finished"
	default:
		return fmt.Sprintf("status(%d)", uint8(s))
	}
}

// JoinRequest asks for a seat. Joining with a PlayerID that already holds a seat
// is a reconnect: the same seat is returned and the player is marked present.
type JoinRequest struct {
	Player PlayerID
}

// JoinResult reports the seat awarded and the state the player is joining into.
type JoinResult struct {
	// Seat is the engine seat index this player occupies.
	Seat engine.PlayerID
	// Reconnect is true when the player already held this seat.
	Reconnect bool
	// State is the room state immediately after the join.
	State Snapshot
}

// MoveRequest is a player's attempt to act. Type/Card/Cell mirror engine.Move;
// MoveID and ExpectedSeq are the delivery-safety fields the engine knows nothing
// about.
type MoveRequest struct {
	Player PlayerID
	// MoveID is a client-generated id, unique per player. Required. Retrying a
	// move with the same id is safe: the room replays the original ack.
	MoveID string
	// ExpectedSeq is the game version the client believes it is acting on.
	// Zero means "unchecked" — a live game always has Seq >= 1, so zero is an
	// unambiguous sentinel for clients that do not track versions yet.
	ExpectedSeq uint64

	Type engine.MoveType
	Card engine.Card
	Cell engine.Cell
}

// MoveResult acknowledges an accepted move.
type MoveResult struct {
	// Seq is the game version AFTER this move (the version it produced, for a
	// fresh move; the version it produced back then, for a duplicate).
	Seq uint64
	// Duplicate is true when this MoveID had already been applied and the room
	// replayed the original acknowledgement without touching the state.
	Duplicate bool
	// State is the room state as of right now. For a duplicate that is the
	// current state, not the historical one — which is what a client retrying
	// after a dropped connection actually wants.
	State Snapshot
}

// LeaveRequest removes a player from a room.
type LeaveRequest struct {
	Player PlayerID
}

// seat is one chair at the table.
type seat struct {
	player   PlayerID
	occupied bool
	// present is false after a Leave during play. The seat is HELD rather than
	// freed so the same PlayerID can reconnect into the same hand (B3's
	// reconnect path); a match with an empty seat is unresumable.
	present bool
}

// Room is one match: an engine.GameState plus seat bookkeeping, owned by a
// single goroutine and driven by a command channel.
type Room struct {
	id     string
	logger *slog.Logger

	cmds      chan command
	quit      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once

	// ---- Everything below is owned exclusively by the run goroutine. ----
	// No other goroutine may read or write these fields; that is the whole
	// point of the actor. Snapshot() copies them out under the run goroutine.

	game   *engine.GameState
	status Status
	seats  []seat

	// seq is the game state version: 1 after the deal, +1 per applied move.
	// Presence changes (join/leave) deliberately do NOT bump it — a player
	// reconnecting must not invalidate another player's in-flight legal move.
	seq uint64

	// applied maps "player/move_id" to the seq that move produced. This is the
	// deduplication table for at-least-once delivery. Only ACCEPTED moves are
	// recorded: a rejected move had no effect, so a retry should be re-judged
	// rather than served a cached rejection. (B4 logs exactly this same set of
	// accepted commands.)
	applied map[string]uint64
}

// Options configures a new room.
type Options struct {
	// ID is the room's identifier. Manager assigns it.
	ID string
	// Game is passed straight to engine.NewGame.
	Game engine.Options
	// Logger receives room lifecycle events. Optional.
	Logger *slog.Logger
	// Buffer is the command channel's capacity. A small buffer smooths bursts
	// without letting an unbounded backlog build up.
	Buffer int
}

const defaultBuffer = 16

// New creates a room and starts its goroutine. The deck is dealt immediately
// from rng, before anyone joins: the deal is a pure function of the seed, so
// dealing early keeps room creation the only place that can fail and makes the
// whole match reproducible from (seed, room id). Seats are then claimed against
// an already-dealt hand.
//
// The caller must Close the room to stop its goroutine.
func New(rng *rand.Rand, opts Options) (*Room, error) {
	game, err := engine.NewGame(rng, opts.Game)
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", opts.ID, err)
	}
	return start(game, opts), nil
}

// start wraps an already-built game in a room and launches its goroutine. Split
// out from New so tests can start a room around a scripted position.
func start(game *engine.GameState, opts Options) *Room {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	buf := opts.Buffer
	if buf <= 0 {
		buf = defaultBuffer
	}

	r := &Room{
		id:      opts.ID,
		logger:  logger.With("room", opts.ID),
		cmds:    make(chan command, buf),
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
		game:    game,
		status:  StatusWaiting,
		seats:   make([]seat, game.NumPlayers),
		seq:     1,
		applied: make(map[string]uint64),
	}
	go r.run()
	return r
}

// ID returns the room's identifier.
func (r *Room) ID() string { return r.id }

// Close stops the room's goroutine. It is safe to call more than once and from
// any goroutine; in-flight callers get ErrRoomClosed.
func (r *Room) Close() {
	r.closeOnce.Do(func() {
		close(r.quit)
		<-r.stopped
		r.logger.Debug("room closed")
	})
}

// Done returns a channel closed once the room's goroutine has stopped.
func (r *Room) Done() <-chan struct{} { return r.stopped }

// Join claims (or reclaims) a seat.
func (r *Room) Join(ctx context.Context, req JoinRequest) (JoinResult, error) {
	rep, err := r.do(ctx, command{kind: cmdJoin, join: req})
	return rep.join, err
}

// PlayMove submits a move. Rule violations come back as engine sentinel errors
// (engine.ErrNotYourTurn, engine.ErrCellOccupied, …) and leave the state
// untouched, because engine.Apply validates before it mutates.
func (r *Room) PlayMove(ctx context.Context, req MoveRequest) (MoveResult, error) {
	rep, err := r.do(ctx, command{kind: cmdMove, move: req})
	return rep.move, err
}

// Leave removes a player. Before the game starts the seat is freed for someone
// else; during play the seat is held and merely marked absent so the player can
// reconnect into the same hand.
func (r *Room) Leave(ctx context.Context, req LeaveRequest) error {
	_, err := r.do(ctx, command{kind: cmdLeave, leave: req})
	return err
}

// Snapshot returns a deep copy of the room state. Copying is not an
// optimization detail — it is what keeps the actor's invariant true. Handing out
// the live maps and slices would let a caller read them while the run goroutine
// writes, which is precisely the data race the design exists to prevent.
func (r *Room) Snapshot(ctx context.Context) (Snapshot, error) {
	rep, err := r.do(ctx, command{kind: cmdSnapshot})
	return rep.snap, err
}

// --- command plumbing -------------------------------------------------------

type commandKind uint8

const (
	cmdJoin commandKind = iota
	cmdMove
	cmdLeave
	cmdSnapshot
)

// command is one unit of work for the run goroutine. It is a plain data struct
// rather than a closure so that B4 can serialize accepted commands to the
// write-ahead log without having to reverse-engineer a function value.
type command struct {
	kind  commandKind
	join  JoinRequest
	move  MoveRequest
	leave LeaveRequest
	reply chan reply
}

// reply carries whichever result the command produced, plus its error.
type reply struct {
	join JoinResult
	move MoveResult
	snap Snapshot
	err  error
}

// do sends a command and waits for its reply, honouring both the caller's
// context and room shutdown at each step.
func (r *Room) do(ctx context.Context, c command) (reply, error) {
	// Buffered so the run goroutine can always deliver the reply and move on,
	// even if the caller's context expired and nobody is listening any more.
	// An unbuffered channel here would wedge the whole room on one dead client.
	c.reply = make(chan reply, 1)

	select {
	case r.cmds <- c:
	case <-r.stopped:
		return reply{}, ErrRoomClosed
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}

	select {
	case rep := <-c.reply:
		return rep, rep.err
	case <-r.stopped:
		return reply{}, ErrRoomClosed
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
}

// run is the room's single owning goroutine: the only code allowed to touch the
// game state.
func (r *Room) run() {
	defer close(r.stopped)
	for {
		select {
		case <-r.quit:
			return
		case c := <-r.cmds:
			c.reply <- r.handle(c)
		}
	}
}

// handle dispatches one command. Runs on the run goroutine.
func (r *Room) handle(c command) reply {
	switch c.kind {
	case cmdJoin:
		res, err := r.handleJoin(c.join)
		return reply{join: res, err: err}
	case cmdMove:
		res, err := r.handleMove(c.move)
		return reply{move: res, err: err}
	case cmdLeave:
		return reply{err: r.handleLeave(c.leave)}
	case cmdSnapshot:
		return reply{snap: r.snapshot()}
	default:
		return reply{err: fmt.Errorf("room: unknown command kind %d", c.kind)}
	}
}

func (r *Room) handleJoin(req JoinRequest) (JoinResult, error) {
	if req.Player == "" {
		return JoinResult{}, ErrUnknownPlayer
	}

	// Reconnect: the player already holds a seat. Idempotent by design — a
	// client that retries a join after a timeout gets the same seat back rather
	// than consuming a second one.
	if s, ok := r.seatOf(req.Player); ok {
		r.seats[s].present = true
		r.logger.Debug("player reconnected", "player", req.Player, "seat", s)
		return JoinResult{Seat: s, Reconnect: true, State: r.snapshot()}, nil
	}

	for i := range r.seats {
		if r.seats[i].occupied {
			continue
		}
		r.seats[i] = seat{player: req.Player, occupied: true, present: true}
		if r.allSeated() && r.status == StatusWaiting {
			r.status = StatusPlaying
			r.logger.Info("game started", "players", len(r.seats))
		}
		r.logger.Debug("player joined", "player", req.Player, "seat", i)
		return JoinResult{Seat: engine.PlayerID(i), State: r.snapshot()}, nil
	}
	return JoinResult{}, ErrRoomFull
}

func (r *Room) handleMove(req MoveRequest) (MoveResult, error) {
	if req.MoveID == "" {
		return MoveResult{}, ErrMissingMoveID
	}
	seat, ok := r.seatOf(req.Player)
	if !ok {
		return MoveResult{}, ErrNotInRoom
	}

	// Duplicate check comes before every other validation. A retry of an already
	// applied move must succeed even if the state has since moved on (it is no
	// longer this player's turn, the cell is now occupied, …) — otherwise a
	// dropped ack turns a legal move into a spurious rule violation.
	key := dedupeKey(req.Player, req.MoveID)
	if at, dup := r.applied[key]; dup {
		return MoveResult{Seq: at, Duplicate: true, State: r.snapshot()}, nil
	}

	if r.status == StatusWaiting {
		return MoveResult{}, ErrGameNotStarted
	}
	if req.ExpectedSeq != 0 && req.ExpectedSeq != r.seq {
		return MoveResult{}, ErrStaleSeq
	}

	// The engine owns every rule from here down, including "not your turn" and
	// "game is over". It validates fully before mutating, so an error leaves the
	// state untouched and we record nothing.
	if err := r.game.Apply(engine.Move{
		Player: seat,
		Type:   req.Type,
		Card:   req.Card,
		Cell:   req.Cell,
	}); err != nil {
		return MoveResult{}, err
	}

	r.seq++
	r.applied[key] = r.seq
	if r.game.GameOver() {
		r.status = StatusFinished
		r.logger.Info("game finished", "winner", r.game.Winner, "moves", r.seq-1)
	}
	return MoveResult{Seq: r.seq, State: r.snapshot()}, nil
}

func (r *Room) handleLeave(req LeaveRequest) error {
	s, ok := r.seatOf(req.Player)
	if !ok {
		return ErrNotInRoom
	}
	if r.status == StatusWaiting {
		// Nothing has been played yet, so the seat carries no history: free it
		// outright and let someone else take it.
		r.seats[s] = seat{}
		r.logger.Debug("player left before start", "player", req.Player, "seat", s)
		return nil
	}
	// Mid-game: hold the seat, drop presence. See seat.present.
	r.seats[s].present = false
	r.logger.Debug("player disconnected", "player", req.Player, "seat", s)
	return nil
}

// seatOf finds a player's seat. Runs on the run goroutine.
func (r *Room) seatOf(p PlayerID) (engine.PlayerID, bool) {
	for i := range r.seats {
		if r.seats[i].occupied && r.seats[i].player == p {
			return engine.PlayerID(i), true
		}
	}
	return engine.NoPlayer, false
}

// allSeated reports whether every seat is taken.
func (r *Room) allSeated() bool {
	for i := range r.seats {
		if !r.seats[i].occupied {
			return false
		}
	}
	return true
}

// dedupeKey scopes a move id to its player, so two clients independently
// numbering their moves 1, 2, 3, … never collide.
func dedupeKey(p PlayerID, moveID string) string {
	return string(p) + "\x00" + moveID
}
