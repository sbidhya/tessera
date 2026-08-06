package room

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(config.Config{Seed: 42}, nil)
	t.Cleanup(m.Close)
	return m
}

func TestManagerCreateAndGet(t *testing.T) {
	m := testManager(t)

	r, err := m.Create(engine.Options{NumPlayers: 2})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ID() != "room-1" {
		t.Errorf("first room id = %q, want room-1", r.ID())
	}

	got, ok := m.Get(r.ID())
	if !ok || got != r {
		t.Errorf("Get(%q) = %v, %v; want the created room", r.ID(), got, ok)
	}
	if _, ok := m.Get("room-nope"); ok {
		t.Error("Get of an unknown id should report false")
	}
	if m.Len() != 1 {
		t.Errorf("Len = %d, want 1", m.Len())
	}
}

func TestManagerRejectsBadGameOptions(t *testing.T) {
	m := testManager(t)
	if _, err := m.Create(engine.Options{NumPlayers: 5}); err == nil {
		t.Error("unsupported player count should fail at creation")
	}
	if m.Len() != 0 {
		t.Errorf("a failed Create left %d rooms registered", m.Len())
	}
}

func TestManagerListAndRemove(t *testing.T) {
	m := testManager(t)

	ids := make(map[string]bool)
	for range 3 {
		r, err := m.Create(engine.Options{NumPlayers: 2})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[r.ID()] = true
	}
	listed := m.List()
	if len(listed) != 3 {
		t.Fatalf("List returned %d ids, want 3", len(listed))
	}
	for _, id := range listed {
		if !ids[id] {
			t.Errorf("List returned unexpected id %q", id)
		}
	}

	victim := listed[0]
	if err := m.Remove(victim); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := m.Get(victim); ok {
		t.Error("removed room is still registered")
	}
	if err := m.Remove(victim); !errors.Is(err, ErrNoSuchRoom) {
		t.Errorf("second Remove err = %v, want ErrNoSuchRoom", err)
	}
	if m.Len() != 2 {
		t.Errorf("Len = %d, want 2", m.Len())
	}
}

func TestManagerRemoveStopsRoom(t *testing.T) {
	m := testManager(t)
	r, err := m.Create(engine.Options{NumPlayers: 2})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Remove(r.ID()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := r.Snapshot(context.Background()); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("removed room err = %v, want ErrRoomClosed", err)
	}
}

func TestManagerCloseShutsEverythingDown(t *testing.T) {
	m := NewManager(config.Config{Seed: 1}, nil)
	var rooms []*Room
	for range 3 {
		r, err := m.Create(engine.Options{NumPlayers: 2})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		rooms = append(rooms, r)
	}

	m.Close()
	m.Close() // idempotent

	if m.Len() != 0 {
		t.Errorf("Len after Close = %d, want 0", m.Len())
	}
	for _, r := range rooms {
		select {
		case <-r.Done():
		default:
			t.Errorf("room %s still running after manager Close", r.ID())
		}
	}
	if _, err := m.Create(engine.Options{NumPlayers: 2}); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Create after Close err = %v, want ErrManagerClosed", err)
	}
}

// TestManagerConcurrentCreate checks the registry under contention: ids must be
// unique and no room may be lost. Run under -race.
func TestManagerConcurrentCreate(t *testing.T) {
	m := testManager(t)

	const n = 50
	created := make([]*Room, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Create(engine.Options{NumPlayers: 2})
			if err != nil {
				return
			}
			created[i] = r
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, r := range created {
		if r == nil {
			t.Fatalf("Create %d failed", i)
		}
		if seen[r.ID()] {
			t.Fatalf("duplicate room id %q", r.ID())
		}
		seen[r.ID()] = true
	}
	if m.Len() != n {
		t.Errorf("Len = %d, want %d", m.Len(), n)
	}
}

// TestManagerRoomsAreReproducible pins the "whole process from one integer"
// property: the same seed yields the same deal in the same room id.
func TestManagerRoomsAreReproducible(t *testing.T) {
	deal := func() []engine.Card {
		m := NewManager(config.Config{Seed: 99}, nil)
		defer m.Close()
		r, err := m.Create(engine.Options{NumPlayers: 2})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		s, err := r.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return s.Hands[0]
	}

	a, b := deal(), deal()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("room-1 deal differs at %d for the same seed: %v vs %v", i, a[i], b[i])
		}
	}
}
