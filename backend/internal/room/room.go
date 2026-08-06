package room

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// PlayResult is the authoritative outcome of a PlayMove attempt.
//
// Seq is the room's monotonic sequence after handling the command. On success it
// is incremented by one; on duplicate it is the original seq; on error it is
// the current seq (no increment). Callers can use Seq as the next expectedSeq
// for idempotent retries and for staleness detection.
//
// Duplicate reports whether this move_id was seen before. Duplicate moves are
// not re-applied — their original result/error is returned and the board state
// is not changed.
type PlayResult struct {
	Seq       uint64
	Duplicate bool
	State     *engine.GameState
}

// internal command types — each carries a 1-element buffered response channel so
// the actor never blocks on reply.

type cmdJoin struct {
	token string
	resp  chan joinResp
}
type joinResp struct {
	player engine.PlayerID
	err    error
}

type cmdLeave struct {
	token string
	resp  chan error
}

type cmdPlay struct {
	token       string
	move        engine.Move
	moveID      string
	expectedSeq uint64
	resp        chan playResp
}
type playResp struct {
	result PlayResult
	err    error
}

type cmdState struct {
	resp chan stateResp
}
type stateResp struct {
	state *engine.GameState
	seq   uint64
}

type cmdSeq struct {
	resp chan uint64
}

type cmdClose struct {
	resp chan error
}

// seenEntry caches the response for a move_id so retries are idempotent without
// re-applying engine logic (required for B4 WAL replay as well).
type seenEntry struct {
	result PlayResult
	err    error
}

// Room is one match: a single authoritative GameState owned by one goroutine.
// All mutations go through the command channel (actor model). Callers must not
// access the GameState directly — use State(), PlayMove(), etc., which send
// commands to the owner goroutine.
//
// The hot path holds no mutex: the goroutine is the sole owner of the
// GameState, join map, sequence counter, and deduplication cache.
type Room struct {
	id     string
	logger *slog.Logger

	cmds chan any
	done chan struct{}
	once sync.Once
}

// ID returns the room's identifier.
func (r *Room) ID() string { return r.id }

// newRoom creates a room, starts its actor, and returns it. The caller (usually
// Manager.CreateRoom) is responsible for inserting it into a registry if needed.
func newRoom(id string, gs *engine.GameState, logger *slog.Logger) *Room {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Room{
		id:     id,
		logger: logger.With("room", id),
		cmds:   make(chan any, 64),
		done:   make(chan struct{}),
	}
	go r.loop(gs)
	return r
}

// loop is the actor: the sole owner of gs, players, seq, and dedup cache.
func (r *Room) loop(gs *engine.GameState) {
	defer close(r.done)

	players := make(map[string]engine.PlayerID) // token -> seat
	used := make([]bool, gs.NumPlayers)         // seat occupancy
	// seq counts successful state-changing moves applied.
	var seq uint64
	seen := make(map[string]seenEntry)

	for cmd := range r.cmds {
		switch c := cmd.(type) {
		case cmdJoin:
			c.resp <- handleJoin(c, gs, players, used)

		case cmdLeave:
			c.resp <- handleLeave(c, players, used)

		case cmdPlay:
			// Idempotency: if moveID was seen, return cached outcome immediately
			// without touching engine state or sequence. This makes retries safe
			// and WAL replay idempotent.
			if c.moveID != "" {
				if entry, ok := seen[c.moveID]; ok {
					dup := entry.result
					dup.Duplicate = true
					// Clone state snapshot again so the caller cannot mutate cache.
					if dup.State != nil {
						dup.State = dup.State.Clone()
					}
					c.resp <- playResp{result: dup, err: entry.err}
					continue
				}
			}
			res, err := handlePlay(c, gs, players, seq)
			// Cache only SUCCESSFUL moves: a failed move (stale seq, illegal,
			// etc.) should not poison the moveID. The client may retry the same
			// logical move with a corrected expectedSeq and a different validity;
			// caching failures would make the retry artificially fail. Successful
			// moves are cached so that a retry after a lost response returns the
			// original success even though the retry's expectedSeq is now stale.
			if err == nil && c.moveID != "" {
				cached := res
				if cached.State != nil {
					cached.State = cached.State.Clone()
				}
				seen[c.moveID] = seenEntry{result: cached, err: nil}
				seq = res.Seq
			}
			c.resp <- playResp{result: res, err: err}

		case cmdState:
			c.resp <- stateResp{state: gs.Clone(), seq: seq}

		case cmdSeq:
			c.resp <- seq

		case cmdClose:
			c.resp <- nil
			return

		default:
			r.logger.Error("unknown command", "type", 0)
		}
	}
}

func handleJoin(c cmdJoin, gs *engine.GameState, players map[string]engine.PlayerID, used []bool) joinResp {
	if pid, ok := players[c.token]; ok {
		return joinResp{player: pid, err: nil}
	}
	// Find next free seat.
	seat := -1
	for i, u := range used {
		if !u {
			seat = i
			break
		}
	}
	if seat == -1 {
		return joinResp{player: engine.NoPlayer, err: ErrRoomFull}
	}
	pid := engine.PlayerID(seat)
	players[c.token] = pid
	used[seat] = true
	return joinResp{player: pid, err: nil}
}

func handleLeave(c cmdLeave, players map[string]engine.PlayerID, used []bool) error {
	pid, ok := players[c.token]
	if !ok {
		return ErrNotInRoom
	}
	delete(players, c.token)
	if int(pid) < len(used) {
		used[pid] = false
	}
	return nil
}

func handlePlay(c cmdPlay, gs *engine.GameState, players map[string]engine.PlayerID, seq uint64) (PlayResult, error) {
	if c.moveID == "" {
		return PlayResult{Seq: seq, State: gs.Clone()}, ErrInvalidMoveID
	}
	pid, ok := players[c.token]
	if !ok {
		return PlayResult{Seq: seq, State: gs.Clone()}, ErrNotInRoom
	}
	if c.expectedSeq != seq {
		return PlayResult{Seq: seq, State: gs.Clone()}, ErrStaleSequence
	}
	if c.move.Player != pid {
		return PlayResult{Seq: seq, State: gs.Clone()}, ErrPlayerMismatch
	}
	// Delegate to the pure engine. Apply validates fully before mutating, so a
	// failure leaves gs unchanged — the room can surface the sentinel directly.
	if err := gs.Apply(c.move); err != nil {
		// Map engine sentinels through unchanged so callers can errors.Is.
		return PlayResult{Seq: seq, State: gs.Clone()}, err
	}
	// Success.
	nextSeq := seq + 1
	return PlayResult{Seq: nextSeq, State: gs.Clone()}, nil
}

// Join assigns the token to a seat. Idempotent: re-joining with the same token
// returns the previously assigned PlayerID.
func (r *Room) Join(token string) (engine.PlayerID, error) {
	if token == "" {
		return engine.NoPlayer, errors.New("room: empty token")
	}
	select {
	case <-r.done:
		return engine.NoPlayer, ErrRoomClosed
	default:
	}
	resp := make(chan joinResp, 1)
	select {
	case r.cmds <- cmdJoin{token: token, resp: resp}:
	case <-r.done:
		return engine.NoPlayer, ErrRoomClosed
	}
	select {
	case jr := <-resp:
		return jr.player, jr.err
	case <-r.done:
		return engine.NoPlayer, ErrRoomClosed
	}
}

// Leave removes the token from the room, freeing its seat.
func (r *Room) Leave(token string) error {
	select {
	case <-r.done:
		return ErrRoomClosed
	default:
	}
	resp := make(chan error, 1)
	select {
	case r.cmds <- cmdLeave{token: token, resp: resp}:
	case <-r.done:
		return ErrRoomClosed
	}
	select {
	case err := <-resp:
		return err
	case <-r.done:
		return ErrRoomClosed
	}
}

// PlayMove is the idempotent, ordered move entry point.
//
//   - moveID must be non-empty and stable per attempt: retries with the same
//     moveID return the cached result without re-applying the move.
//   - expectedSeq must equal the room's current sequence (from Seq() or the
//     last PlayResult.Seq). A mismatch yields ErrStaleSequence — the client
//     should refresh state and retry with the fresh seq.
//   - The caller must have previously Joined; move.Player must equal the seat
//     assigned to token or ErrPlayerMismatch is returned.
//
// The move is applied atomically by the room actor via engine.GameState.Apply.
// Out-of-turn, illegal-card, occupied-cell, jack, and dead-card errors are the
// engine sentinels (e.g. engine.ErrNotYourTurn) returned verbatim so callers
// can use errors.Is.
func (r *Room) PlayMove(token string, move engine.Move, moveID string, expectedSeq uint64) (PlayResult, error) {
	select {
	case <-r.done:
		return PlayResult{}, ErrRoomClosed
	default:
	}
	resp := make(chan playResp, 1)
	cmd := cmdPlay{token: token, move: move, moveID: moveID, expectedSeq: expectedSeq, resp: resp}
	select {
	case r.cmds <- cmd:
	case <-r.done:
		return PlayResult{}, ErrRoomClosed
	}
	select {
	case pr := <-resp:
		return pr.result, pr.err
	case <-r.done:
		return PlayResult{}, ErrRoomClosed
	}
}

// State returns a deep copy of the authoritative GameState and the current
// sequence number. The caller may read/mutate the snapshot freely.
func (r *Room) State() (*engine.GameState, uint64) {
	select {
	case <-r.done:
		return nil, 0
	default:
	}
	resp := make(chan stateResp, 1)
	select {
	case r.cmds <- cmdState{resp: resp}:
	case <-r.done:
		return nil, 0
	}
	select {
	case sr := <-resp:
		return sr.state, sr.seq
	case <-r.done:
		return nil, 0
	}
}

// Seq returns the current monotonic sequence (successful moves applied). It is
// safe to use as the expectedSeq for the next PlayMove.
func (r *Room) Seq() uint64 {
	select {
	case <-r.done:
		return 0
	default:
	}
	resp := make(chan uint64, 1)
	select {
	case r.cmds <- cmdSeq{resp: resp}:
	case <-r.done:
		return 0
	}
	select {
	case s := <-resp:
		return s
	case <-r.done:
		return 0
	}
}

// Close stops the actor goroutine. It is idempotent. Manager.DeleteRoom calls
// this; direct callers (tests) may also call it.
func (r *Room) Close() error {
	r.once.Do(func() {
		resp := make(chan error, 1)
		// Best-effort close command; if the channel is already closed the select
		// below will fall through to direct close.
		select {
		case r.cmds <- cmdClose{resp: resp}:
			<-resp
		default:
		}
		close(r.cmds)
		<-r.done
	})
	return nil
}
