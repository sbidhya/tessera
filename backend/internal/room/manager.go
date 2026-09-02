package room

import (
	"cmp"
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

// DefaultFinishedRetention is how long an archived terminal room remains live
// for immediate reconnects before Manager evicts it.
const DefaultFinishedRetention = 5 * time.Minute

// Timer is the stoppable timer subset Manager needs. It is public so tests and
// embedders can provide a deterministic Clock.
type Timer interface {
	Stop() bool
}

// Clock schedules retention expiry. Production uses the wall clock; tests can
// advance a fake clock without sleeping.
type Clock interface {
	AfterFunc(time.Duration, func()) Timer
}

type wallClock struct{}

func (wallClock) AfterFunc(delay time.Duration, fn func()) Timer {
	return time.AfterFunc(delay, fn)
}

// ManagerOptions controls room lifecycle policy. Zero values select production
// defaults.
type ManagerOptions struct {
	FinishedRetention time.Duration
	Clock             Clock
}

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
	clock   Clock
	retain  time.Duration

	mu             sync.Mutex
	rooms          map[string]*Room
	evictionTimers map[string]Timer
	onEvicted      func(string)
	ids            *rand.Rand
	closed         bool
}

// NewManager builds an empty manager. randFor supplies each room's RNG stream,
// so with a fixed process seed the whole set of matches is reproducible.
func NewManager(logger *slog.Logger, randFor RandFunc) *Manager {
	return newManager(logger, randFor, nil, nil, ManagerOptions{})
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
	return NewPersistentManagerWithOptions(logger, randFor, journal, archive, ManagerOptions{})
}

// NewPersistentManagerWithOptions is NewPersistentManager with an injectable
// lifecycle policy. It is used by the server for configured retention and by
// tests for a manually advanced clock.
func NewPersistentManagerWithOptions(logger *slog.Logger, randFor RandFunc, journal EventJournal, archive FinishedMatchSink, opts ManagerOptions) (*Manager, error) {
	if journal == nil {
		return nil, fmt.Errorf("room: durable manager requires an event journal")
	}
	m := newManager(logger, randFor, journal, archive, opts)
	if err := m.recover(); err != nil {
		return nil, err
	}
	return m, nil
}

func newManager(logger *slog.Logger, randFor RandFunc, journal EventJournal, archive FinishedMatchSink, opts ManagerOptions) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.FinishedRetention <= 0 {
		opts.FinishedRetention = DefaultFinishedRetention
	}
	if opts.Clock == nil {
		opts.Clock = wallClock{}
	}
	return &Manager{
		logger:         logger,
		randFor:        randFor,
		journal:        journal,
		archive:        archive,
		clock:          opts.Clock,
		retain:         opts.FinishedRetention,
		rooms:          make(map[string]*Room),
		evictionTimers: make(map[string]Timer),
		ids:            randFor("room-ids"),
	}
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
	r.archived = func(finishedSeq uint64) { m.markArchived(r, finishedSeq) }
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
		r.archived = func(finishedSeq uint64) { m.markArchived(r, finishedSeq) }
		recovered = append(recovered, r)
	}
	for _, r := range recovered {
		// Build the projection before the actor starts, while direct field access
		// is still safe. Register and start the room before enqueueing it so even a
		// very fast cold-tier acknowledgement can find a live actor to retain.
		var finished *FinishedMatch
		if r.status == StatusFinished && r.archive != nil {
			match := r.finishedMatch()
			finished = &match
		}
		m.rooms[r.id] = r
		r.start()
		if finished != nil {
			r.enqueueFinishedMatch(*finished)
		}
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

// SetEvictionHandler installs the process-local hook invoked when retention
// evicts a room. Transport uses it to tear down the corresponding hub. The
// handler must not call back into Manager and is run without m.mu held.
func (m *Manager) SetEvictionHandler(handler func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvicted = handler
}

// Close stops one room and unregisters it.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	r, ok := m.rooms[id]
	delete(m.rooms, id)
	timer := m.evictionTimers[id]
	delete(m.evictionTimers, id)
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchRoom, id)
	}
	if timer != nil {
		timer.Stop()
	}
	// Closed outside the lock: Close waits for the room's goroutine to drain,
	// and holding the directory lock across that wait would let one slow room
	// block every lookup in the process.
	r.Close()
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
	timers := make([]Timer, 0, len(m.evictionTimers))
	for _, timer := range m.evictionTimers {
		timers = append(timers, timer)
	}
	m.evictionTimers = make(map[string]Timer)
	m.mu.Unlock()

	for _, timer := range timers {
		timer.Stop()
	}
	for _, r := range rooms {
		r.Close()
	}
	m.logger.Info("room manager shut down", "rooms_closed", len(rooms))
}

// markArchived starts the grace window only after the cold tier confirms its
// durable write. Duplicate acknowledgements are harmless and do not extend an
// already-running retention window.
func (m *Manager) markArchived(r *Room, finishedSeq uint64) {
	m.mu.Lock()
	if m.closed || m.rooms[r.id] != r || m.evictionTimers[r.id] != nil {
		m.mu.Unlock()
		return
	}
	m.evictionTimers[r.id] = m.clock.AfterFunc(m.retain, func() {
		m.evictArchived(r, finishedSeq)
	})
	m.mu.Unlock()
	m.logger.Info("finished room retained", "room", r.id, "seq", finishedSeq, "retention", m.retain)
}

// evictArchived removes exactly the room whose archival acknowledgement
// scheduled this callback. Deleting under the directory lock prevents a stale
// callback from evicting any future room that happens to reuse the same id.
func (m *Manager) evictArchived(r *Room, finishedSeq uint64) {
	m.mu.Lock()
	if m.closed || m.rooms[r.id] != r {
		m.mu.Unlock()
		return
	}
	delete(m.rooms, r.id)
	delete(m.evictionTimers, r.id)
	handler := m.onEvicted
	m.mu.Unlock()

	r.Close()
	if handler != nil {
		handler(r.id)
	}
	m.logger.Info("finished room evicted", "room", r.id, "seq", finishedSeq)
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
