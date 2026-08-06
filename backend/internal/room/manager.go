package room

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// Manager is the in-memory registry of rooms. It owns the map from room ID to
// *Room; each Room owns its own GameState via a single actor goroutine. The
// manager's mutex protects only the registry map — not the game states — so the
// hot path (PlayMove inside a room) never contends on this lock.
//
// Layering: transport (B3) depends on room; room depends on engine and config;
// engine depends on nothing.
type Manager struct {
	cfg    config.Config
	logger *slog.Logger

	mu     sync.RWMutex
	rooms  map[string]*Room
	nextID uint64
}

// NewManager creates a manager that derives per-room RNGs from cfg. Every room
// gets a statistically independent stream NewRand("room-<id>") while the whole
// process stays reproducible from the single cfg.Seed.
func NewManager(cfg config.Config) *Manager {
	logger := cfg.Logger()
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:    cfg,
		logger: logger,
		rooms:  make(map[string]*Room),
	}
}

// CreateRoom creates a new room hosting a fresh 2-player game (or as configured
// by opts). The room's board and deck are both derived from the per-room RNG
// stream so identical seeds recreate identical games — needed for B4 WAL replay
// determinism.
//
// The returned *Room is already running its actor goroutine. The caller should
// eventually call Manager.DeleteRoom or Room.Close.
func (m *Manager) CreateRoom(opts engine.Options) (*Room, error) {
	m.mu.Lock()
	id := fmt.Sprintf("room-%d", m.nextID)
	m.nextID++
	m.mu.Unlock()

	rng := m.cfg.NewRand("room-" + id)
	gs, err := engine.NewGame(rng, opts)
	if err != nil {
		return nil, err
	}
	r := newRoom(id, gs, m.logger)

	m.mu.Lock()
	m.rooms[id] = r
	m.mu.Unlock()

	m.logger.Info("room created", "room", id, "numPlayers", opts.NumPlayers)
	return r, nil
}

// GetRoom returns the room with the given id, and whether it exists.
func (m *Manager) GetRoom(id string) (*Room, bool) {
	m.mu.RLock()
	r, ok := m.rooms[id]
	m.mu.RUnlock()
	return r, ok
}

// DeleteRoom stops and removes the room. It returns true if the room existed.
func (m *Manager) DeleteRoom(id string) bool {
	m.mu.Lock()
	r, ok := m.rooms[id]
	if ok {
		delete(m.rooms, id)
	}
	m.mu.Unlock()
	if ok {
		_ = r.Close()
		m.logger.Info("room deleted", "room", id)
	}
	return ok
}

// RoomCount returns the number of live rooms (for health checks / tests).
func (m *Manager) RoomCount() int {
	m.mu.RLock()
	n := len(m.rooms)
	m.mu.RUnlock()
	return n
}

// ListIDs returns a snapshot of live room IDs.
func (m *Manager) ListIDs() []string {
	m.mu.RLock()
	ids := make([]string, 0, len(m.rooms))
	for id := range m.rooms {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	return ids
}
