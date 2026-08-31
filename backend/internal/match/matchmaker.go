// Package match pairs waiting players into rooms and tracks who is online.
//
// It is the B6 lobby layer, sitting between the room manager and transport:
// engine ← room ← match ← transport. Like room, the matchmaker is an actor —
// one goroutine owns the waiting queue, fed by a command channel — so pairing
// two waiters into a room is atomic and never interleaves with a cancel. The
// queue itself is process-local memory by design (the brief says "in-memory
// matchmaking queue"): a crash drops waiters, and clients simply re-queue with
// backoff. Durability is unaffected — once paired, the room is created through
// room.Manager, so the match gets the same WAL record and replay as a
// directly created one.
//
// Presence is deliberately separate from the room's per-seat Present flag.
// The room tracks "does this seat's occupant have a live socket in THIS
// match"; Presence tracks "does this player hold ANY live socket". A player in
// two matches is one online player, not two, which is why the tracker
// refcounts connections per player id.
package match

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// ErrMatchmakerClosed is returned by Join when the matchmaker has been shut
// down. Waiters are never stranded: Close fails every pending join.
var ErrMatchmakerClosed = errors.New("match: matchmaker is closed")

// ErrInvalidSequencesToWin is returned for a negative sequences_to_win. Zero
// means "default" and is normalised to 2, mirroring the engine and the REST
// create-match endpoint.
var ErrInvalidSequencesToWin = errors.New("match: sequences_to_win must be positive")

// ErrLeftQueue reports that a queued join ended because the player left the
// queue (explicit Cancel), not because the caller's context ended. Join
// returns it to whoever is still blocked in the long-poll so HTTP can answer
// 204 "your wait ended without a match" instead of a misleading 500.
var ErrLeftQueue = errors.New("match: player left the matchmaking queue")

// DefaultSequencesToWin is the lobby default when the client expresses no
// preference.
const DefaultSequencesToWin = 2

// pairTimeout bounds the room-Join round-trips performed while pairing. Pairing
// runs on the matchmaker's single goroutine, so an unresponsive room must not
// stall the whole queue; on timeout the half-built room is closed and both
// waiters fail loudly rather than hanging.
const pairTimeout = 10 * time.Second

// Request is one player's wish to be paired into a match.
type Request struct {
	// PlayerID is the verified anonymous identity (transport authenticates the
	// token before this layer ever sees the request).
	PlayerID string
	// SequencesToWin is the preferred match length. 0 selects the default;
	// negative is rejected. Only waiters with equal preferences are paired.
	SequencesToWin int
}

// Result is a completed pairing. Both players receive the same MatchID and
// their own Seat; the room is already started (both seats are joined before
// either waiter is released), so clients can open their sockets immediately.
type Result struct {
	MatchID string
	Seat    engine.PlayerID
}

// waiter is one queued join. done is closed when the waiter leaves the queue
// for any reason; reply carries the outcome exactly once.
type waiter struct {
	playerID       string
	sequencesToWin int
	reply          chan waitOutcome
	done           chan struct{}
}

type waitOutcome struct {
	result Result
	err    error
}

// ops are the matchmaker mailbox messages. Every op runs on the loop
// goroutine, which is the only code that touches waiters, byPlayer, or seq.
type joinOp struct {
	req Request
	ack chan *waiter
}

type cancelOp struct {
	playerID string
	ack      chan cancelOutcome
}

type cancelOutcome struct {
	// found is true when the player had a queue entry at all.
	found bool
	// paired is true when the waiter was already matched: the cancel lost the
	// race and the caller should use the pairing result instead of treating
	// the context error as authoritative.
	paired bool
	result Result
}

// recentCap bounds the pairing-result ring. It only needs to cover the race
// window between "partner found" and "caller gave up": 64 entries is ample,
// and the bound keeps a busy lobby from growing memory without limit.
const recentCap = 64

type recentPair struct {
	playerID string
	result   Result
}

type depthOp struct {
	reply chan int
}

type closeOp struct {
	ack chan struct{}
}

// Matchmaker pairs queued players into rooms. All methods are safe for
// concurrent use; Close is idempotent.
type Matchmaker struct {
	logger *slog.Logger
	rooms  *room.Manager

	ops  chan any
	done chan struct{}

	closeOnce sync.Once

	// ---- Owned exclusively by the loop goroutine. ----
	waiters  []*waiter
	byPlayer map[string]*waiter
	// recent is a ring of the latest pairing results. If a caller's context
	// ends in the same instant their partner is found, the waiter is already
	// gone from byPlayer but the match is real and the player is seated: the
	// cancel path recovers the result here instead of stranding a ghost seat
	// the client can never learn about. Entries are never invalidated; a live
	// waiter in byPlayer always shadows a stale ring entry, and a ring entry
	// for a previous lobby visit is only ever read by a cancel that already
	// queued a fresh waiter — which byPlayer then handles first.
	recent     [recentCap]recentPair
	recentNext int
	closed     bool
}

// NewMatchmaker builds a matchmaker that creates rooms through rooms and
// starts its queue goroutine.
func NewMatchmaker(logger *slog.Logger, rooms *room.Manager) *Matchmaker {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Matchmaker{
		logger:   logger,
		rooms:    rooms,
		ops:      make(chan any, 64),
		done:     make(chan struct{}),
		byPlayer: make(map[string]*waiter),
	}
	go m.loop()
	return m
}

func (m *Matchmaker) loop() {
	defer close(m.done)
	for {
		select {
		case op := <-m.ops:
			switch op := op.(type) {
			case joinOp:
				op.ack <- m.handleJoin(op.req)
			case cancelOp:
				op.ack <- m.handleCancel(op.playerID)
			case depthOp:
				op.reply <- len(m.waiters)
			case closeOp:
				m.failAllLocked()
				m.closed = true
				close(op.ack)
				return
			}
		}
	}
}

// Join queues the player and blocks until a partner arrives, the context ends,
// or the matchmaker closes. Joining twice while already queued attaches to the
// existing queue entry: a client that times out and retries cannot end up
// queued twice or paired into two matches.
func (m *Matchmaker) Join(ctx context.Context, req Request) (Result, error) {
	if req.PlayerID == "" {
		return Result{}, room.ErrInvalidPlayerID
	}
	stw := req.SequencesToWin
	if stw == 0 {
		stw = DefaultSequencesToWin
	}
	if stw < 0 {
		return Result{}, ErrInvalidSequencesToWin
	}
	req.SequencesToWin = stw

	ack := make(chan *waiter, 1)
	select {
	case m.ops <- joinOp{req: req, ack: ack}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-m.done:
		return Result{}, ErrMatchmakerClosed
	}

	var w *waiter
	select {
	case w = <-ack:
	case <-ctx.Done():
		// The loop may still be processing our join; withdraw it before
		// reporting the context error so a retry starts from a clean queue.
		// handleCancel reports whether pairing already won the race.
		return m.unwrapCancel(ctx, req.PlayerID)
	case <-m.done:
		return Result{}, ErrMatchmakerClosed
	}

	select {
	case out := <-w.reply:
		return out.result, out.err
	case <-ctx.Done():
		return m.unwrapCancel(ctx, w.playerID)
	case <-m.done:
		// Close fails every waiter through w.reply before done closes, so this
		// arm is unreachable in practice; it exists so a caller can never
		// block forever if the loop ever exits without replying.
		select {
		case out := <-w.reply:
			return out.result, out.err
		default:
			return Result{}, ErrMatchmakerClosed
		}
	}
}

// unwrapCancel withdraws a waiter after the caller's context ended and turns
// the outcome into a Join return. If pairing won the race, holding a live
// match is success — the late context error is discarded.
func (m *Matchmaker) unwrapCancel(ctx context.Context, playerID string) (Result, error) {
	err := m.cancelAndReport(ctx, playerID)
	var paired errPaired
	if errors.As(err, &paired) {
		return paired.result, nil
	}
	return Result{}, err
}

// cancelAndReport withdraws a queued waiter after the caller's context ended.
// If pairing already completed, the pairing wins: the caller holds a live
// match and must use it.
func (m *Matchmaker) cancelAndReport(ctx context.Context, playerID string) error {
	ack := make(chan cancelOutcome, 1)
	select {
	case m.ops <- cancelOp{playerID: playerID, ack: ack}:
	case <-m.done:
		return ErrMatchmakerClosed
	}
	select {
	case out := <-ack:
		if out.paired {
			m.logger.Info("matchmaker: cancel lost to pairing", "player", playerID, "match", out.result.MatchID)
			return errPaired{result: out.result}
		}
		return ctx.Err()
	case <-m.done:
		return ErrMatchmakerClosed
	}
}

// rememberLocked records a pairing result in the recent ring. Runs on the
// loop goroutine.
func (m *Matchmaker) rememberLocked(playerID string, result Result) {
	m.recent[m.recentNext] = recentPair{playerID: playerID, result: result}
	m.recentNext = (m.recentNext + 1) % recentCap
}

// recallLocked returns the latest pairing result for a player with no live
// waiter. Runs on the loop goroutine. The scan runs newest-first: a player
// who paired in an earlier lobby visit may own several ring entries, and only
// the latest can belong to the Join that is being cancelled now.
func (m *Matchmaker) recallLocked(playerID string) (Result, bool) {
	for i := 0; i < recentCap; i++ {
		slot := (m.recentNext - 1 - i + 2*recentCap) % recentCap
		if m.recent[slot].playerID == playerID {
			return m.recent[slot].result, true
		}
	}
	return Result{}, false
}

// errPaired reports a successful pairing to a caller whose context already
// ended. Join unwraps it into a normal (Result, nil) return so callers do not
// need a new error branch: receiving a match is success, however late.
type errPaired struct {
	result Result
}

func (e errPaired) Error() string { return "match: paired into " + e.result.MatchID }

// Cancel withdraws a player from the queue without pairing. It reports whether
// a waiter was actually removed.
func (m *Matchmaker) Cancel(ctx context.Context, playerID string) (bool, error) {
	ack := make(chan cancelOutcome, 1)
	select {
	case m.ops <- cancelOp{playerID: playerID, ack: ack}:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-m.done:
		return false, ErrMatchmakerClosed
	}
	select {
	case out := <-ack:
		return out.found && !out.paired, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-m.done:
		return false, ErrMatchmakerClosed
	}
}

// QueueDepth reports how many players are currently waiting.
func (m *Matchmaker) QueueDepth(ctx context.Context) (int, error) {
	reply := make(chan int, 1)
	select {
	case m.ops <- depthOp{reply: reply}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-m.done:
		return 0, ErrMatchmakerClosed
	}
	select {
	case n := <-reply:
		return n, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-m.done:
		return 0, ErrMatchmakerClosed
	}
}

// Close rejects every queued waiter and stops the loop. It is idempotent.
//
// The ops channel is never closed: like room, callers select a send against
// done and the loop's return, so a send racing with Close resolves to
// ErrMatchmakerClosed instead of panicking on a closed channel.
func (m *Matchmaker) Close() {
	m.closeOnce.Do(func() {
		ack := make(chan struct{})
		select {
		case m.ops <- closeOp{ack: ack}:
			<-ack
		case <-m.done:
		}
	})
	<-m.done
}

// failAllLocked rejects every queued waiter. Runs on the loop goroutine.
func (m *Matchmaker) failAllLocked() {
	for _, w := range m.waiters {
		delete(m.byPlayer, w.playerID)
		w.reply <- waitOutcome{err: ErrMatchmakerClosed}
		close(w.done)
	}
	m.waiters = nil
}

// handleJoin registers (or reuses) a waiter and tries to pair it. Runs on the
// loop goroutine.
func (m *Matchmaker) handleJoin(req Request) *waiter {
	if m.closed {
		w := &waiter{playerID: req.PlayerID, reply: make(chan waitOutcome, 1), done: make(chan struct{})}
		w.reply <- waitOutcome{err: ErrMatchmakerClosed}
		close(w.done)
		return w
	}
	if w, ok := m.byPlayer[req.PlayerID]; ok {
		return w
	}
	w := &waiter{
		playerID:       req.PlayerID,
		sequencesToWin: req.SequencesToWin,
		reply:          make(chan waitOutcome, 1),
		done:           make(chan struct{}),
	}
	m.waiters = append(m.waiters, w)
	m.byPlayer[req.PlayerID] = w
	m.logger.Info("matchmaker: player queued", "player", req.PlayerID,
		"sequences_to_win", req.SequencesToWin, "depth", len(m.waiters))
	m.tryPair(w)
	return w
}

// handleCancel removes a waiter that has not been paired yet. If the player
// has no live waiter but a recent pairing result, the pairing won the race
// and its result is reported so the caller can use the match.
func (m *Matchmaker) handleCancel(playerID string) cancelOutcome {
	w, ok := m.byPlayer[playerID]
	if !ok {
		if result, paired := m.recallLocked(playerID); paired {
			return cancelOutcome{found: true, paired: true, result: result}
		}
		return cancelOutcome{}
	}
	m.remove(w)
	// No reply can be pending here: the loop is the only writer of w.reply
	// and every write removes the waiter from byPlayer first, so a waiter
	// still registered has never been answered.
	// ErrLeftQueue (not context.Canceled): the caller's context may still be
	// alive — they left the queue on purpose via Cancel, which is an answer,
	// not an interruption. The reply channel is buffered, so this never
	// blocks even when the Join caller already went away via its own context.
	w.reply <- waitOutcome{err: ErrLeftQueue}
	close(w.done)
	m.logger.Info("matchmaker: player left queue", "player", playerID, "depth", len(m.waiters))
	return cancelOutcome{found: true}
}

// tryPair matches a newly queued waiter with the earliest compatible waiter.
// Compatibility is equal sequences_to_win: a fast test game (1) must not
// absorb a player who asked for a full match (2).
func (m *Matchmaker) tryPair(w *waiter) {
	var partner *waiter
	for _, other := range m.waiters {
		if other != w && other.sequencesToWin == w.sequencesToWin {
			partner = other
			break
		}
	}
	if partner == nil {
		return
	}
	result, err := m.pair(partner, w)
	if err != nil {
		m.logger.Warn("matchmaker: pairing failed", "players", []string{partner.playerID, w.playerID}, "err", err)
		// Removed before replying, like the success path: a waiter still in
		// byPlayer never has a reply pending, which is what handleCancel
		// relies on.
		m.remove(partner)
		m.remove(w)
		partner.reply <- waitOutcome{err: err}
		close(partner.done)
		w.reply <- waitOutcome{err: err}
		close(w.done)
		return
	}
	m.remove(partner)
	m.remove(w)
	partnerResult := Result{MatchID: result, Seat: 0}
	wResult := Result{MatchID: result, Seat: 1}
	m.rememberLocked(partner.playerID, partnerResult)
	m.rememberLocked(w.playerID, wResult)
	partner.reply <- waitOutcome{result: partnerResult}
	close(partner.done)
	w.reply <- waitOutcome{result: wResult}
	close(w.done)
	m.logger.Info("matchmaker: players paired", "match", result,
		"players", []string{partner.playerID, w.playerID})
}

// pair creates the room and seats both players. Seats are assigned in queue
// order (earlier waiter gets seat 0) so the outcome is deterministic. Both
// joins happen before either waiter is released, so a paired client always
// observes a started match.
func (m *Matchmaker) pair(first, second *waiter) (string, error) {
	r, err := m.rooms.Create(engine.Options{NumPlayers: 2, SequencesToWin: first.sequencesToWin})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairTimeout)
	defer cancel()
	if _, err := r.Join(ctx, first.playerID); err != nil {
		_ = m.rooms.Close(r.ID())
		return "", err
	}
	secondJoin, err := r.Join(ctx, second.playerID)
	if err != nil {
		_ = m.rooms.Close(r.ID())
		return "", err
	}
	if secondJoin.Seat != 1 {
		_ = m.rooms.Close(r.ID())
		return "", errors.New("match: unexpected seat assignment while pairing")
	}
	return r.ID(), nil
}

func (m *Matchmaker) remove(w *waiter) {
	delete(m.byPlayer, w.playerID)
	for i, other := range m.waiters {
		if other == w {
			m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
			return
		}
	}
}
