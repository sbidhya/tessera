package room

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// Manager is the registry of live rooms.
//
// This is the one place in the design that does use a mutex — and that is the
// point of the split. The registry is a cold path (a handful of map operations
// when a match is created, looked up, or reaped), whereas gameplay is the hot
// path and stays lock-free inside each room's goroutine. Holding a lock only
// long enough to read a map pointer means one match can never stall another.
//
// The lock is never held while a command is sent to a room, so a slow or wedged
// room cannot block room creation elsewhere.
type Manager struct {
	cfg    config.Config
	logger *slog.Logger

	mu     sync.Mutex
	rooms  map[string]*Room
	nextID int
	closed bool
}

// NewManager creates an empty registry. cfg supplies the single process seed
// that every room's RNG is derived from.
func NewManager(cfg config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		cfg:    cfg,
		logger: logger,
		rooms:  make(map[string]*Room),
	}
}

// Create starts a new room and registers it.
//
// Room ids are a simple monotonic counter ("room-1", "room-2", …) rather than
// random strings, and each room's RNG is config.NewRand(id). Two consequences
// worth having: the whole process is reproducible from one integer seed, and
// each room gets a statistically independent stream, so concurrent shuffles
// never alias onto one another.
func (m *Manager) Create(game engine.Options) (*Room, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	m.nextID++
	id := fmt.Sprintf("room-%d", m.nextID)
	m.mu.Unlock()

	// Constructed outside the lock: dealing a game is pure computation, but
	// there is no reason to serialize every match creation behind it.
	r, err := New(m.cfg.NewRand(id), Options{
		ID:     id,
		Game:   game,
		Logger: m.logger,
	})
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		// The manager shut down while we were constructing. Do not leak the
		// goroutine we just started.
		m.mu.Unlock()
		r.Close()
		return nil, ErrManagerClosed
	}
	m.rooms[id] = r
	m.mu.Unlock()

	m.logger.Info("room created", "room", id, "players", game.NumPlayers)
	return r, nil
}

// Get looks up a live room by id.
func (m *Manager) Get(id string) (*Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	return r, ok
}

// List returns the ids of all live rooms, in no particular order.
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.rooms))
	for id := range m.rooms {
		ids = append(ids, id)
	}
	return ids
}

// Len reports how many rooms are live.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// Remove closes a room and unregisters it. Returns ErrNoSuchRoom if the id is
// unknown.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	r, ok := m.rooms[id]
	if ok {
		delete(m.rooms, id)
	}
	m.mu.Unlock()

	if !ok {
		return ErrNoSuchRoom
	}
	// Closed outside the lock: Close waits for the room's goroutine to drain,
	// which must not block the registry.
	r.Close()
	m.logger.Info("room removed", "room", id)
	return nil
}

// Close shuts down every room and refuses further creation. Safe to call twice.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
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
	m.logger.Info("room manager closed", "rooms", len(rooms))
}
