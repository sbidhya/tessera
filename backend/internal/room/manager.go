package room

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// RandFunc returns an independent, deterministic RNG stream for a named
// subsystem. config.Config.NewRand satisfies it.
//
// It is a function type rather than a *config.Config so this package depends on
// engine and the standard library only — the layering stays a straight line and
// tests can inject any RNG they like.
type RandFunc func(stream string) *rand.Rand

// Manager is the process-wide registry of live rooms.
//
// It uses a plain mutex, which is not a contradiction of the lock-free actor
// model: the lock guards the *directory* (create/lookup/delete), which is
// touched once per match rather than once per move. The hot path — every move
// in every match — still runs lock-free inside each room's goroutine.
type Manager struct {
	logger  *slog.Logger
	randFor RandFunc

	mu     sync.Mutex
	rooms  map[string]*Room
	ids    *rand.Rand
	closed bool
}

// NewManager builds an empty manager. randFor supplies each room's RNG stream,
// so with a fixed process seed the whole set of matches is reproducible.
func NewManager(logger *slog.Logger, randFor RandFunc) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:  logger,
		randFor: randFor,
		rooms:   make(map[string]*Room),
		ids:     randFor("room-ids"),
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
	r, err := New(id, m.logger, m.randFor("room:"+id), opts)
	if err != nil {
		return nil, err
	}
	m.rooms[id] = r
	m.logger.Info("room created", "room", id, "players", opts.NumPlayers,
		"sequences_to_win", opts.SequencesToWin)
	return r, nil
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
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchRoom, id)
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
