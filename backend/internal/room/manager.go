package room

import (
	"cmp"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// RandFunc returns an independent, deterministic RNG stream for a named
// subsystem. config.Config.NewRand satisfies it.
//
// It is a function type rather than a *config.Config so this package depends on
// engine and the standard library only — the layering stays a straight line and
// tests can inject any RNG they like.
type RandFunc func(stream string) *rand.Rand

// DefaultRetention is the grace window a finished, archived match stays live
// before Sweep evicts it. It mirrors config.DefaultRoomRetention (kept as a
// literal here so room keeps importing engine and the standard library only).
// Five minutes covers an immediate reconnect after the winning move without
// keeping every finished match's goroutine, idempotency map, and event
// history alive forever.
const DefaultRetention = 5 * time.Minute

// Manager is the process-wide registry of live rooms.
//
// It uses a plain mutex, which is not a contradiction of the lock-free actor
// model: the lock guards the *directory* (create/lookup/delete), which is
// touched once per match rather than once per move. The hot path — every move
// in every match — still runs lock-free inside each room's goroutine.
type Manager struct {
	logger  *slog.Logger
	randFor RandFunc
	journal EventJournal
	archive FinishedMatchSink

	mu     sync.Mutex
	rooms  map[string]*Room
	ids    *rand.Rand
	closed bool

	// Retention policy for finished matches. retention is the grace window
	// after the winning move; now is the injectable clock (time.Now in
	// production, a fake in tests); archived records ids the cold tier has
	// durably stored (via NotifyArchived); onEvict runs after each eviction
	// so transport can tear down the match's hub (P0-1 coordination).
	retention time.Duration
	now       func() time.Time
	archived  map[string]bool
	onEvict   func(roomID string)
}

// NewManager builds an empty manager. randFor supplies each room's RNG stream,
// so with a fixed process seed the whole set of matches is reproducible.
func NewManager(logger *slog.Logger, randFor RandFunc) *Manager {
	return newManager(logger, randFor, nil, nil)
}

// NewDurableManager builds a manager backed by journal and replays every
// recorded room before returning. No room goroutine starts until the complete
// log has validated, so a corrupt WAL cannot expose partially recovered state.
func NewDurableManager(logger *slog.Logger, randFor RandFunc, journal EventJournal) (*Manager, error) {
	return NewPersistentManager(logger, randFor, journal, nil)
}

// NewPersistentManager builds a durable manager and sends each completed match
// to archive. Recovery also re-emits a terminal match whose WAL survived a
// crash before SQLite was committed; the cold tier's idempotent transaction
// makes that retry safe.
func NewPersistentManager(logger *slog.Logger, randFor RandFunc, journal EventJournal, archive FinishedMatchSink) (*Manager, error) {
	if journal == nil {
		return nil, fmt.Errorf("room: durable manager requires an event journal")
	}
	m := newManager(logger, randFor, journal, archive)
	if err := m.recover(); err != nil {
		return nil, err
	}
	return m, nil
}

func newManager(logger *slog.Logger, randFor RandFunc, journal EventJournal, archive FinishedMatchSink) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:    logger,
		randFor:   randFor,
		journal:   journal,
		archive:   archive,
		rooms:     make(map[string]*Room),
		ids:       randFor("room-ids"),
		retention: DefaultRetention,
		now:       time.Now,
		archived:  make(map[string]bool),
	}
}

// SetRetention configures the finished-match grace window and the clock Sweep
// uses to measure it. A non-positive ttl selects DefaultRetention; a nil clock
// selects time.Now. It exists so tests can inject a fake clock and advance
// past the window without sleeping, while production passes
// config.RoomRetention with the wall clock.
func (m *Manager) SetRetention(ttl time.Duration, now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ttl <= 0 {
		ttl = DefaultRetention
	}
	if now == nil {
		now = time.Now
	}
	m.retention = ttl
	m.now = now
}

// SetEvictHook registers fn to run after each retention eviction (and each
// explicit Close, which can also orphan a hub). Transport wires its hub
// teardown here so evicting a room also retires its hub. A nil fn disables
// the hook. Called outside the directory lock, after the room goroutine has
// exited.
func (m *Manager) SetEvictHook(fn func(roomID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvict = fn
}

// NotifyArchived records that id has been durably stored in the SQLite cold
// tier. It is called by the store worker after its commit (see
// store.Options.OnArchived), not by the room actor at enqueue time: eviction
// requires the commit, not just the queue. It is idempotent and safe to call
// before the room has finished (the flag waits for the finish timestamp).
func (m *Manager) NotifyArchived(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.archived[id] = true
}

// Sweep evicts every room whose match finished at least retention ago AND has
// been archived. Rooms without a cold tier (nil archive sink) treat the
// finish itself as archived. Sweep is explicit rather than timer-driven so
// tests can drive it with a fake clock; production runs it on a ticker (see
// cmd/tessera) plus after each store commit.
func (m *Manager) Sweep() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	now := m.now()
	ttl := m.retention
	if ttl <= 0 {
		ttl = DefaultRetention
	}
	m.mu.Unlock()

	for _, r := range rooms {
		snap, err := r.Snapshot(context.Background(), "")
		if err != nil {
			continue // closed concurrently; directory already dropped it
		}
		if snap.Status != StatusFinished || snap.FinishedAt.IsZero() {
			continue
		}
		if now.Before(snap.FinishedAt.Add(ttl)) {
			continue // still in the reconnect grace window
		}
		m.mu.Lock()
		archived := m.archive == nil || m.archived[r.id]
		m.mu.Unlock()
		if !archived {
			continue // SQLite has not committed this match yet
		}
		m.evict(r.id)
	}
}

// evict unregisters id, stops its goroutine (which frees its applied map and
// events slice on the owning goroutine), and runs the evict hook. It is a
// no-op for unknown ids so concurrent Close/Sweep pairs cannot fail.
func (m *Manager) evict(id string) {
	m.mu.Lock()
	r, ok := m.rooms[id]
	if ok {
		delete(m.rooms, id)
	}
	delete(m.archived, id)
	hook := m.onEvict
	m.mu.Unlock()

	if !ok {
		return
	}
	// Closed outside the lock for the same reason Close documents: Close
	// waits for the room goroutine, which must not block directory lookups.
	r.Close()
	if hook != nil {
		hook(id)
	}
	m.logger.Info("room evicted", "room", id)
}

// Create starts a new room and registers it.
//
// The room's game RNG is drawn from the stream named after its id, so two rooms
// created in the same process never share a shuffle, and re-running the process
// with the same seed reproduces the same ids AND the same deals.
func (m *Manager) Create(opts engine.Options) (*Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}

	id := m.newIDLocked()
	seedSource := m.randFor("room:" + id)
	seed := [2]uint64{seedSource.Uint64(), seedSource.Uint64()}
	r, err := newRoom(id, m.logger, rngFromSeed(seed), opts, m.journal)
	if err != nil {
		return nil, err
	}
	created := createdEvent(id, engine.Options{
		NumPlayers:     r.gs.NumPlayers,
		SequencesToWin: r.gs.SequencesToWin,
	}, seed)
	if err := r.append(created); err != nil {
		return nil, err
	}
	r.archive = m.archive
	r.now = m.now
	r.start()
	m.rooms[id] = r
	m.logger.Info("room created", "room", id, "players", r.gs.NumPlayers,
		"sequences_to_win", r.gs.SequencesToWin)
	return r, nil
}

func (m *Manager) recover() error {
	events, err := m.journal.ReadAll()
	if err != nil {
		return fmt.Errorf("room: read journal: %w", err)
	}
	byRoom := make(map[string][]Event)
	for _, event := range events {
		if err := event.validate(); err != nil {
			return fmt.Errorf("room: replay: %w", err)
		}
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	ids := make([]string, 0, len(byRoom))
	for id := range byRoom {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	recovered := make([]*Room, 0, len(ids))
	for _, id := range ids {
		r, err := replayRoom(id, m.logger, byRoom[id])
		if err != nil {
			return fmt.Errorf("room: replay %s: %w", id, err)
		}
		r.journal = m.journal
		r.archive = m.archive
		r.now = m.now
		if r.status == StatusFinished {
			// The WAL carries no finish timestamp, so anchor the grace
			// window at recovery: a crash must not evict a finished match
			// before clients have had a chance to reconnect, nor resurrect
			// it forever. Replay's playMove stamped finishedAt with the
			// standalone clock; overwrite it with the manager clock.
			r.finishedAt = m.now()
		}
		recovered = append(recovered, r)
	}
	for _, r := range recovered {
		// Notify before the actor starts, while direct access to its fields is
		// still safe. A finished WAL left behind by a crash is thereby queued for
		// the same idempotent SQLite write as a newly completed match.
		if r.status == StatusFinished && r.archive != nil {
			r.archive.MatchFinished(r.finishedMatch())
		}
		r.start()
		m.rooms[r.id] = r
	}
	if len(recovered) > 0 {
		m.logger.Info("rooms recovered", "count", len(recovered))
	}
	return nil
}

func replayRoom(id string, logger *slog.Logger, events []Event) (*Room, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("empty event stream")
	}
	created := events[0]
	if created.Type != EventRoomCreated || created.Seq != 1 || created.RoomID != id {
		return nil, fmt.Errorf("first event must create room at seq 1")
	}
	r, err := newRoom(id, logger, rngFromSeed(created.RNGSeed), created.Options, nil)
	if err != nil {
		return nil, err
	}
	seen := map[uint64]Event{created.Seq: created}
	r.events = append(r.events, created)

	for _, event := range events[1:] {
		if previous, ok := seen[event.Seq]; ok {
			if previous != event {
				return nil, fmt.Errorf("conflicting events at seq %d", event.Seq)
			}
			continue
		}
		if event.Seq != r.seq+1 {
			return nil, fmt.Errorf("event sequence gap: room at %d, next event is %d", r.seq, event.Seq)
		}

		switch event.Type {
		case EventPlayerJoined:
			if r.status == StatusFinished {
				if err := r.replayFinishedPresence(event); err != nil {
					return nil, fmt.Errorf("apply legacy %s at seq %d: %w", event.Type, event.Seq, err)
				}
				break
			}
			result, err := r.join(event.PlayerID)
			if err != nil {
				return nil, fmt.Errorf("apply %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if result.Seq != event.Seq {
				return nil, fmt.Errorf("apply %s produced seq %d, want %d", event.Type, result.Seq, event.Seq)
			}
		case EventMoveApplied:
			result, err := r.playMove(event.Move)
			if err != nil {
				return nil, fmt.Errorf("apply %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if result.Duplicate || result.Seq != event.Seq {
				return nil, fmt.Errorf("apply %s produced duplicate=%t seq=%d, want false/%d",
					event.Type, result.Duplicate, result.Seq, event.Seq)
			}
		case EventPlayerLeft:
			if r.status == StatusFinished {
				if err := r.replayFinishedPresence(event); err != nil {
					return nil, fmt.Errorf("apply legacy %s at seq %d: %w", event.Type, event.Seq, err)
				}
				break
			}
			if err := r.leave(event.PlayerID); err != nil {
				return nil, fmt.Errorf("apply %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if r.seq != event.Seq {
				return nil, fmt.Errorf("apply %s produced seq %d, want %d", event.Type, r.seq, event.Seq)
			}
		case EventRoomCreated:
			return nil, fmt.Errorf("unexpected create event at seq %d", event.Seq)
		default:
			return nil, fmt.Errorf("unknown event type %q", event.Type)
		}
		seen[event.Seq] = event
	}

	// Network presence is process-local. A crash disconnects every socket even
	// if the final durable event said a player was present; seats and game state
	// remain, but clients must explicitly rejoin after restart.
	for i := range r.seats {
		if r.seats[i].playerID != "" {
			r.seats[i].present = false
		}
	}
	return r, nil
}

func rngFromSeed(seed [2]uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed[0], seed[1]))
}

// Get looks up a room by id.
func (m *Manager) Get(id string) (*Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	return r, ok
}

// List returns every live room, ordered by id so callers and tests see a stable
// order.
func (m *Manager) List() []*Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	slices.SortFunc(rooms, func(a, b *Room) int { return cmp.Compare(a.id, b.id) })
	return rooms
}

// Close stops one room and unregisters it.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	r, ok := m.rooms[id]
	delete(m.rooms, id)
	delete(m.archived, id)
	hook := m.onEvict
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchRoom, id)
	}
	// Closed outside the lock: Close waits for the room's goroutine to drain,
	// and holding the directory lock across that wait would let one slow room
	// block every lookup in the process.
	r.Close()
	if hook != nil {
		hook(id)
	}
	m.logger.Info("room closed", "room", id)
	return nil
}

// Shutdown stops every room and refuses further Creates. It is idempotent, and
// is what a graceful process shutdown calls.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.rooms = make(map[string]*Room)
	m.archived = make(map[string]bool)
	m.mu.Unlock()

	for _, r := range rooms {
		r.Close()
	}
	m.logger.Info("room manager shut down", "rooms_closed", len(rooms))
}

// newIDLocked mints an unused room id. Caller must hold m.mu.
//
// Ids are random rather than sequential so they are not guessable — a room id
// is effectively a capability to join a match until real auth arrives in B6 —
// but they come from the seeded stream, so a fixed seed still reproduces them.
func (m *Manager) newIDLocked() string {
	var b [6]byte
	for {
		for i := range b {
			b[i] = byte(m.ids.UintN(256))
		}
		id := "r_" + hex.EncodeToString(b[:])
		if _, taken := m.rooms[id]; !taken {
			return id
		}
	}
}
