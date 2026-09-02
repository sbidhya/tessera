package room

import (
	"errors"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// TestFinishedRoomEvictedAfterRetention is the retention regression gate: a
// finished, archived match stays reachable through the grace window, then
// Sweep evicts it — the directory drops it, the actor goroutine exits, and
// the per-match memory (idempotency map, event history) is released. The
// clock is injected and advanced; the test never sleeps.
func TestFinishedRoomEvictedAfterRetention(t *testing.T) {
	journal := &memoryJournal{}
	archive := &captureArchive{matches: make(chan FinishedMatch, 4)}
	cfg := config.Config{Seed: 77}
	m, err := NewPersistentManager(testLogger(), cfg.NewRand, journal, archive)
	if err != nil {
		t.Fatalf("NewPersistentManager: %v", err)
	}
	t.Cleanup(m.Shutdown)

	const ttl = 5 * time.Minute
	now := time.Now()
	m.SetRetention(ttl, func() time.Time { return now })
	evicted := make(chan string, 2)
	m.SetEvictHook(func(id string) { evicted <- id })

	r, err := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	finishTestRoom(t, r)

	select {
	case <-archive.matches:
	case <-time.After(5 * time.Second):
		t.Fatal("finished match was never enqueued to the archive sink")
	}
	final := mustSnapshot(t, r, "alice")
	if final.Status != StatusFinished || final.FinishedAt.IsZero() {
		t.Fatalf("finished snapshot = %+v, want finished with a finish timestamp", final)
	}

	// Still in the grace window and not yet archived: Sweep must keep the
	// final snapshot reachable for an immediate reconnect.
	m.Sweep()
	if _, ok := m.Get(r.ID()); !ok {
		t.Fatal("room evicted before the grace window passed")
	}
	if _, err := r.Snapshot(t.Context(), "alice"); err != nil {
		t.Fatalf("Snapshot in grace window: %v", err)
	}

	// Archived but still in the grace window: still reachable.
	m.NotifyArchived(r.ID())
	m.Sweep()
	if _, ok := m.Get(r.ID()); !ok {
		t.Fatal("room evicted before the grace window passed (archived early)")
	}

	// Past the window, archived: eligible. Sweep must drop the directory
	// entry, stop the goroutine, and run the hub-teardown hook.
	now = now.Add(ttl + time.Second)
	m.Sweep()

	if _, ok := m.Get(r.ID()); ok {
		t.Fatal("room still registered after grace window + archive")
	}
	select {
	case <-r.done:
	default:
		t.Fatal("room goroutine did not exit after eviction")
	}
	if _, err := r.Snapshot(t.Context(), "alice"); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("Snapshot after eviction = %v, want %v", err, ErrRoomClosed)
	}
	// Freed on the owning goroutine as it exited (before done closed, so
	// reading here is race-free under -race).
	if r.applied != nil {
		t.Errorf("applied map not freed at eviction (len %d)", len(r.applied))
	}
	if r.events != nil {
		t.Errorf("events history not freed at eviction (len %d)", len(r.events))
	}
	for _, live := range m.List() {
		if live.ID() == r.ID() {
			t.Fatalf("List still contains evicted room %s", r.ID())
		}
	}
	select {
	case id := <-evicted:
		if id != r.ID() {
			t.Fatalf("evict hook fired for %s, want %s", id, r.ID())
		}
	default:
		t.Fatal("evict hook did not run (hub teardown would leak)")
	}
}

// TestFinishedRoomWaitsForArchive pins the AND in the eligibility rule: past
// the grace window but never archived, the room must stay live. Otherwise a
// slow SQLite commit could discard the only copy of the result.
func TestFinishedRoomWaitsForArchive(t *testing.T) {
	journal := &memoryJournal{}
	archive := &captureArchive{matches: make(chan FinishedMatch, 2)}
	cfg := config.Config{Seed: 78}
	m, err := NewPersistentManager(testLogger(), cfg.NewRand, journal, archive)
	if err != nil {
		t.Fatalf("NewPersistentManager: %v", err)
	}
	t.Cleanup(m.Shutdown)

	const ttl = time.Minute
	now := time.Now()
	m.SetRetention(ttl, func() time.Time { return now })

	r, err := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	finishTestRoom(t, r)
	<-archive.matches // enqueued, but never committed: no NotifyArchived

	now = now.Add(ttl + time.Second)
	m.Sweep()
	if _, ok := m.Get(r.ID()); !ok {
		t.Fatal("room evicted without ever being archived")
	}
	select {
	case <-r.done:
		t.Fatal("room goroutine exited without ever being archived")
	default:
	}
}

// TestLiveRoomNeverSwept guards the other side: an unfinished match is never
// eligible no matter how far the clock advances.
func TestLiveRoomNeverSwept(t *testing.T) {
	m := testManager(t, 79)
	now := time.Now()
	m.SetRetention(time.Minute, func() time.Time { return now })

	r, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")

	now = now.Add(time.Hour)
	m.Sweep()
	if _, ok := m.Get(r.ID()); !ok {
		t.Fatal("live match evicted by retention Sweep")
	}
}
