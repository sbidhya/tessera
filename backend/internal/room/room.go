// Package room owns live matches: one Room per match, holding the authoritative
// engine.GameState for that match.
//
// # The actor model, and why
//
// A room's state is mutated by exactly ONE goroutine — the room's own loop — fed
// by a buffered command channel (its "mailbox"). Callers never touch the
// GameState; they send a command and wait for a reply. Nothing on the hot path
// takes a lock.
//
// The alternative — a mutex around GameState — would work, but this shape buys
// three things that matter for a game server:
//
//   - Serialisation for free. Two players' moves cannot interleave halfway
//     through a rules check, so "is it your turn?" and "apply the move" are
//     atomic without anybody remembering to hold a lock.
//   - A single ordering of events. The mailbox order IS the match's history,
//     which is exactly what the write-ahead log (B4) needs to append and what
//     the WebSocket layer (B3) needs to broadcast.
//   - Cheap isolation. Rooms share nothing, so one wedged match cannot stall
//     another, and adding rooms scales across cores with no contention.
//
// The cost is discipline: every value handed back to a caller must be a copy
// (see Snapshot), or the "one goroutine owns the state" invariant leaks. That
// invariant is what `go test -race` checks.
//
// # Layering
//
// room imports engine and the standard library only. It knows nothing about
// HTTP, WebSockets, or storage — those depend on it, not the other way round.
package room

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// mailboxSize is the room command channel's buffer. A small buffer absorbs
// bursts (both players tapping at once, a reconnect storm) without forcing
// senders to block, while still applying backpressure if a room somehow falls
// behind rather than letting an unbounded queue eat memory.
const mailboxSize = 32

// Status is the lifecycle stage of a match.
type Status uint8

const (
	// StatusWaiting means seats are still open; moves are rejected.
	StatusWaiting Status = iota
	// StatusPlaying means every seat is filled and the match is in progress.
	StatusPlaying
	// StatusFinished means the match has a winner.
	StatusFinished
)

// String renders a Status for logs and (later) JSON.
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

// PlayerInfo describes one seat's occupant.
type PlayerInfo struct {
	// ID is the caller-supplied player identity (anonymous ids arrive in B6).
	ID string
	// Seat is the engine seat index this player occupies.
	Seat engine.PlayerID
	// Present is false after Leave while the seat is held open for a reconnect.
	Present bool
}

// JoinResult reports the outcome of a Join.
type JoinResult struct {
	// Seat is the engine seat the player occupies. Stable across reconnects.
	Seat engine.PlayerID
	// Rejoined is true when the player already held this seat: Join is
	// idempotent, which is what makes reconnection (B3) a re-Join and nothing
	// more.
	Rejoined bool
	Seq      uint64
	Status   Status
}

// MoveRequest is a client's attempt to act. The room derives the acting seat
// from PlayerID rather than trusting a seat in the request: the server is
// authoritative, so a client cannot move on another player's behalf.
type MoveRequest struct {
	// PlayerID identifies the sender; it must map to a seat in this room.
	PlayerID string
	// MoveID is a client-generated unique id for this attempt. A retry of the
	// same attempt MUST reuse the id — that is what turns an at-least-once
	// network (a flaky mobile connection resending on timeout) into
	// exactly-once application.
	MoveID string
	// ExpectedSeq is optimistic concurrency control: if non-zero, the move is
	// rejected with ErrStaleSeq unless the room is still at this sequence
	// number. Zero means "apply against whatever the current state is".
	ExpectedSeq uint64

	// The move itself. Cell is unused for engine.MoveDeadCard.
	Type engine.MoveType
	Card engine.Card
	Cell engine.Cell
}

// MoveResult reports the outcome of an accepted move.
type MoveResult struct {
	// Seq is the room's sequence number after the move.
	Seq uint64
	// Duplicate is true when this MoveID had already been applied: the move was
	// NOT applied a second time and these are the original result's values.
	Duplicate bool
	Status    Status
	// Turn is whose turn it is now (unchanged by a dead-card swap, and frozen at
	// the winner on a win).
	Turn   engine.PlayerID
	Winner engine.PlayerID
}

// Snapshot is a read-only, deep-copied view of the match for one viewer. It is
// safe to read and mutate from any goroutine: the room hands out copies, never
// its live state.
//
// The view is per-viewer on purpose — an opponent's hand is hidden (only its
// size is reported). Hiding it here rather than in the transport layer means no
// future endpoint can leak it by forgetting to redact.
type Snapshot struct {
	RoomID string
	Seq    uint64
	Status Status
	Turn   engine.PlayerID
	Winner engine.PlayerID
	// NumPlayers and SequencesToWin are immutable match settings included so
	// outer layers can describe a room without reaching into engine state.
	NumPlayers     int
	SequencesToWin int

	// Viewer is the seat this snapshot was rendered for, or engine.NoPlayer for
	// a spectator (an unknown or empty viewer id).
	Viewer engine.PlayerID
	// Hand is the viewer's own hand; nil for a spectator.
	Hand []engine.Card
	// HandCounts is every seat's hand size, including the viewer's.
	HandCounts map[engine.PlayerID]int

	// Board is immutable once built, so it is shared rather than copied.
	Board *engine.Board

	Chips         map[engine.Cell]engine.Chip
	Sequences     []engine.Sequence
	SequencesWon  map[engine.PlayerID]int
	DrawRemaining int
	Players       []PlayerInfo
}

// seat is one place at the table.
type seat struct {
	// playerID is "" when the seat is vacant.
	playerID string
	// present is false while the occupant is disconnected but still holds the
	// seat (mid-game Leave).
	present bool
}

// moveKey scopes idempotency keys per player, so two clients that happen to
// generate the same MoveID cannot be confused for retries of each other.
type moveKey struct {
	player string
	moveID string
}

// Room is one match. All exported methods are safe to call from any goroutine;
// they submit a command to the room's loop and wait for the reply.
type Room struct {
	// Immutable after construction, so readable from any goroutine.
	id     string
	logger *slog.Logger

	cmds chan command
	// quit is closed by Close to ask the loop to stop; done is closed by the
	// loop as it exits, which is what unblocks callers waiting on a reply.
	quit      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// wal is the durability hook. It is set at construction and never changes
	// after the room starts serving; during WAL replay it is nil so replay
	// does not re-log.
	wal WAL

	// ---- Everything below is owned exclusively by the room goroutine. ----
	// No other goroutine may read or write these fields.
	gs      *engine.GameState
	status  Status
	seq     uint64
	seats   []seat
	bySeat  map[string]engine.PlayerID
	applied map[moveKey]MoveResult
}

// New creates a room and starts its goroutine. rng seeds the match (board layout
// and shuffle), so a room is fully reproducible from its RNG stream; see
// Manager for how streams are derived from the process seed.
//
// The game is dealt eagerly at construction: that makes room creation the single
// place where invalid Options can fail, so once a Room exists it always holds a
// valid GameState. Play is gated on Status, not on whether the deal has happened.
func New(id string, logger *slog.Logger, rng *rand.Rand, opts engine.Options) (*Room, error) {
	return NewWithWAL(id, logger, rng, opts, nil)
}

// NewWithWAL is like New but attaches a WAL hook. When wal is non-nil every
// accepted state change is appended to the log BEFORE it is applied to the
// in-memory state and before it is acked to the caller (write-ahead). If the
// append fails the state is not mutated and the caller receives the error.
func NewWithWAL(id string, logger *slog.Logger, rng *rand.Rand, opts engine.Options, wal WAL) (*Room, error) {
	gs, err := engine.NewGame(rng, opts)
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", id, err)
	}
	if logger == nil {
		logger = slog.Default()
	}

	r := &Room{
		id:      id,
		logger:  logger.With("room", id),
		cmds:    make(chan command, mailboxSize),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		wal:     wal,
		gs:      gs,
		status:  StatusWaiting,
		seq:     1,
		seats:   make([]seat, opts.NumPlayers),
		bySeat:  make(map[string]engine.PlayerID, opts.NumPlayers),
		applied: make(map[moveKey]MoveResult),
	}
	go r.loop()
	return r, nil
}

// SetWAL attaches a WAL to an existing room. It is only safe to call before
// the room serves traffic (during WAL replay wiring); concurrent use while
// moves are in flight would race.
func (r *Room) SetWAL(w WAL) { r.wal = w }

// ID returns the room's identifier. Immutable, so no round-trip is needed.
func (r *Room) ID() string { return r.id }

// Close stops the room's goroutine. It is idempotent and blocks until the loop
// has exited. Commands still in the mailbox are abandoned; their senders get
// ErrRoomClosed rather than hanging.
func (r *Room) Close() {
	r.closeOnce.Do(func() { close(r.quit) })
	<-r.done
}

// loop is the room's single owning goroutine: the only code allowed to touch
// the fields below the ownership line in Room.
func (r *Room) loop() {
	defer close(r.done)
	for {
		select {
		case <-r.quit:
			return
		case c := <-r.cmds:
			c.execute(r)
		}
	}
}

// Join seats a player, or returns their existing seat if they are already in
// this room (making reconnection a plain re-Join). The match starts as soon as
// the last seat is filled.
func (r *Room) Join(ctx context.Context, playerID string) (JoinResult, error) {
	return call(ctx, r, func(reply chan<- result[JoinResult]) command {
		return joinCmd{playerID: playerID, reply: reply}
	})
}

// PlayMove submits a move. Rejections come back as errors: engine rule errors
// (engine.ErrNotYourTurn, engine.ErrCardNotInHand, ...) for illegal play, room
// errors for membership/lifecycle/staleness problems.
func (r *Room) PlayMove(ctx context.Context, req MoveRequest) (MoveResult, error) {
	return call(ctx, r, func(reply chan<- result[MoveResult]) command {
		return moveCmd{req: req, reply: reply}
	})
}

// Leave removes a player. Before the match starts this frees the seat; once it
// is under way the seat is held so the player can reconnect.
func (r *Room) Leave(ctx context.Context, playerID string) error {
	_, err := call(ctx, r, func(reply chan<- result[struct{}]) command {
		return leaveCmd{playerID: playerID, reply: reply}
	})
	return err
}

// Snapshot returns the match state as seen by viewer. An unknown or empty
// viewer id gets the spectator view (no hand). This is the read path B3's
// "GET state for reconnect" will sit on.
func (r *Room) Snapshot(ctx context.Context, viewer string) (Snapshot, error) {
	return call(ctx, r, func(reply chan<- result[Snapshot]) command {
		return snapshotCmd{viewer: viewer, reply: reply}
	})
}

// ---- Handlers. All of these run ON the room goroutine. ----

func (r *Room) join(playerID string) (JoinResult, error) {
	if playerID == "" {
		return JoinResult{}, ErrInvalidPlayerID
	}
	if s, ok := r.bySeat[playerID]; ok {
		// Rejoining always returns the same seat. Transitioning from disconnected
		// to present is observable state, so it advances seq exactly once; another
		// Join while already present is a true no-op. This distinction lets B3
		// broadcast presence changes without assigning two states the same version.
		if !r.seats[s].present {
			if r.wal != nil {
				if err := r.wal.LogJoin(r.id, playerID); err != nil {
					return JoinResult{}, err
				}
			}
			r.seats[s].present = true
			r.bump()
		}
		return JoinResult{Seat: s, Rejoined: true, Seq: r.seq, Status: r.status}, nil
	}
	if r.status == StatusFinished {
		return JoinResult{}, ErrGameFinished
	}

	free := slices.IndexFunc(r.seats, func(s seat) bool { return s.playerID == "" })
	if free < 0 {
		return JoinResult{}, ErrRoomFull
	}
	// Write-ahead: log before mutating. New joins always change state.
	if r.wal != nil {
		if err := r.wal.LogJoin(r.id, playerID); err != nil {
			return JoinResult{}, err
		}
	}
	s := engine.PlayerID(free)
	r.seats[free] = seat{playerID: playerID, present: true}
	r.bySeat[playerID] = s
	r.bump()

	// The match begins the moment the table is full. Cards were already dealt
	// per seat at construction, so there is nothing to do but flip the status.
	if r.occupied() == len(r.seats) {
		r.status = StatusPlaying
	}
	r.logger.Info("player joined", "player", playerID, "seat", s,
		"status", r.status.String(), "seq", r.seq)
	if r.status == StatusPlaying {
		r.logger.Info("match started", "seats", len(r.seats), "seq", r.seq)
	}
	return JoinResult{Seat: s, Seq: r.seq, Status: r.status}, nil
}

func (r *Room) playMove(req MoveRequest) (MoveResult, error) {
	if req.MoveID == "" {
		return MoveResult{}, ErrMissingMoveID
	}
	s, ok := r.bySeat[req.PlayerID]
	if !ok {
		return MoveResult{}, ErrNotSeated
	}

	// The duplicate check comes FIRST, ahead of every other check. A retry can
	// legitimately arrive after the state has moved on — the opponent has since
	// played, or the match has ended — and must still be answered with the
	// original outcome. Checking staleness or game-over first would turn a
	// successful move into a spurious error just because the ack was lost.
	key := moveKey{player: req.PlayerID, moveID: req.MoveID}
	if prev, ok := r.applied[key]; ok {
		prev.Duplicate = true
		return prev, nil
	}

	if r.status == StatusWaiting {
		return MoveResult{}, ErrGameNotStarted
	}
	if req.ExpectedSeq != 0 && req.ExpectedSeq != r.seq {
		return MoveResult{}, fmt.Errorf("%w: room at %d, client expected %d",
			ErrStaleSeq, r.seq, req.ExpectedSeq)
	}

	// Pre-validate on a clone so we only log accepted moves. Apply is
	// transactional, but logging after Apply would leave a window where the
	// in-memory state has advanced but the WAL has not (a crash would lose the
	// move). Validating on a clone lets us log BEFORE mutating the real state.
	clone := r.gs.Clone()
	if err := clone.Apply(engine.Move{Player: s, Type: req.Type, Card: req.Card, Cell: req.Cell}); err != nil {
		return MoveResult{}, err
	}
	// Write-ahead: the move is accepted — persist it before it becomes visible.
	if r.wal != nil {
		if err := r.wal.LogMove(r.id, req); err != nil {
			return MoveResult{}, err
		}
	}
	// Commit: clone already holds the post-move state, so adopt it.
	r.gs = clone

	r.bump()
	if r.gs.GameOver() {
		r.status = StatusFinished
		r.logger.Info("match finished", "winner", r.gs.Winner, "seq", r.seq)
	}
	res := MoveResult{Seq: r.seq, Status: r.status, Turn: r.gs.Turn, Winner: r.gs.Winner}

	// Remember the outcome so a retry replays the ack instead of the move.
	// The map is bounded by the number of accepted moves in one match, which is
	// bounded by the deck; no eviction is needed at this scale.
	r.applied[key] = res
	return res, nil
}

func (r *Room) leave(playerID string) error {
	s, ok := r.bySeat[playerID]
	if !ok {
		return ErrNotSeated
	}
	// WAL is written before the state change, but only when the leave will
	// actually change state. A duplicate leave for an already-absent player
	// (as can happen during a WAL duplicate replay) is a no-op and must not
	// bump seq or produce a second log entry.
	if r.status != StatusWaiting && !r.seats[s].present {
		return nil
	}
	if r.wal != nil {
		if err := r.wal.LogLeave(r.id, playerID); err != nil {
			return err
		}
	}
	if r.status == StatusWaiting {
		// Nothing has happened yet: release the seat so someone else can take it.
		delete(r.bySeat, playerID)
		r.seats[s] = seat{}
	} else {
		// Mid-match (or after it): hold the seat so the player can reconnect.
		// Forfeit-on-timeout is a policy decision for a later block, not a
		// consequence of one dropped socket.
		r.seats[s].present = false
	}
	r.bump()
	r.logger.Info("player left", "player", playerID, "seat", s, "status", r.status.String())
	return nil
}

// snapshot renders the per-viewer view. Every map and slice is copied, because
// the caller receives it on another goroutine while the room keeps mutating the
// original.
func (r *Room) snapshot(viewer string) Snapshot {
	seatOf, seated := r.bySeat[viewer]
	if !seated {
		seatOf = engine.NoPlayer
	}

	snap := Snapshot{
		RoomID:         r.id,
		Seq:            r.seq,
		Status:         r.status,
		Turn:           r.gs.Turn,
		Winner:         r.gs.Winner,
		NumPlayers:     r.gs.NumPlayers,
		SequencesToWin: r.gs.SequencesToWin,
		Viewer:         seatOf,
		HandCounts:     make(map[engine.PlayerID]int, len(r.seats)),
		Board:          r.gs.Board,
		Chips:          maps.Clone(r.gs.Chips),
		Sequences:      slices.Clone(r.gs.Sequences),
		SequencesWon:   maps.Clone(r.gs.SequencesWon),
		DrawRemaining:  len(r.gs.Draw),
		Players:        make([]PlayerInfo, 0, len(r.seats)),
	}
	for i := range r.seats {
		p := engine.PlayerID(i)
		snap.HandCounts[p] = len(r.gs.Hands[p])
		if r.seats[i].playerID != "" {
			snap.Players = append(snap.Players, PlayerInfo{
				ID:      r.seats[i].playerID,
				Seat:    p,
				Present: r.seats[i].present,
			})
		}
	}
	if seated {
		// Only the viewer's own hand crosses the boundary.
		snap.Hand = slices.Clone(r.gs.Hands[seatOf])
	}
	return snap
}

// bump advances the room's state version. Every accepted state change bumps it,
// so a client holding seq N knows it has seen everything up to N — the basis for
// ExpectedSeq checks here and for ordered broadcasts in B3.
func (r *Room) bump() { r.seq++ }

// occupied counts filled seats.
func (r *Room) occupied() int {
	n := 0
	for i := range r.seats {
		if r.seats[i].playerID != "" {
			n++
		}
	}
	return n
}
