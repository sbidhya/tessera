package room

import (
	"sync"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

type fakeTimer struct {
	clock   *fakeClock
	due     time.Time
	fn      func()
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0), timers: make(map[*fakeTimer]struct{})}
}

func (c *fakeClock) AfterFunc(delay time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, due: c.now.Add(delay), fn: fn}
	c.timers[timer] = struct{}{}
	return timer
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped {
		return false
	}
	if _, pending := t.clock.timers[t]; !pending {
		return false
	}
	t.stopped = true
	delete(t.clock.timers, t)
	return true
}

func (c *fakeClock) Advance(elapsed time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(elapsed)
	var due []*fakeTimer
	for timer := range c.timers {
		if !timer.due.After(c.now) {
			delete(c.timers, timer)
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.fn()
	}
}

type controlledArchive struct {
	acknowledgements chan func()
}

func (a *controlledArchive) MatchFinished(_ FinishedMatch, archived func()) {
	a.acknowledgements <- archived
}

// TestFinishedRoomEvictedAfterArchiveRetention is the lifecycle regression:
// finishing alone is insufficient, archival starts a bounded grace window,
// and crossing that window removes the directory entry, exits the actor, and
// releases its per-match history allocations without sleeping in the test.
func TestFinishedRoomEvictedAfterArchiveRetention(t *testing.T) {
	const retention = 3 * time.Minute
	clock := newFakeClock()
	archive := &controlledArchive{acknowledgements: make(chan func(), 1)}
	journal := &memoryJournal{}
	cfg := config.Config{Seed: 81}
	m, err := NewPersistentManagerWithOptions(testLogger(), cfg.NewRand, journal, archive, ManagerOptions{
		FinishedRetention: retention,
		Clock:             clock,
	})
	if err != nil {
		t.Fatalf("NewPersistentManagerWithOptions: %v", err)
	}
	t.Cleanup(m.Shutdown)

	r, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	finishTestRoom(t, r)
	archived := <-archive.acknowledgements

	// The winning move has happened, but SQLite has not acknowledged it yet.
	clock.Advance(10 * retention)
	if got, ok := m.Get(r.ID()); !ok || got != r {
		t.Fatal("finished room was evicted before archival acknowledgement")
	}

	evicted := make(chan string, 1)
	m.SetEvictionHandler(func(id string) { evicted <- id })
	archived()
	clock.Advance(retention - time.Nanosecond)
	if _, ok := m.Get(r.ID()); !ok {
		t.Fatal("archived room was evicted before its grace window expired")
	}

	clock.Advance(time.Nanosecond)
	m.mu.Lock()
	_, held := m.rooms[r.ID()]
	m.mu.Unlock()
	if held {
		t.Fatal("Manager.rooms still holds the room after retention expired")
	}
	select {
	case <-r.done:
	default:
		t.Fatal("room actor goroutine did not exit at eviction")
	}
	if r.applied != nil || r.events != nil {
		t.Fatalf("evicted room retained applied/events: %v/%v", r.applied, r.events)
	}
	select {
	case id := <-evicted:
		if id != r.ID() {
			t.Fatalf("eviction handler received %q, want %q", id, r.ID())
		}
	default:
		t.Fatal("eviction handler was not notified")
	}
}
