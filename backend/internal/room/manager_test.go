package room

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// testManager builds a manager whose RNG streams come from a fixed process seed,
// exactly as the real server will wire it.
func testManager(t *testing.T, seed int64) *Manager {
	t.Helper()
	cfg := config.Config{Seed: seed, LogLevel: slog.LevelError}
	m := NewManager(testLogger(), cfg.NewRand)
	t.Cleanup(m.Shutdown)
	return m
}

var twoPlayer = engine.Options{NumPlayers: 2, SequencesToWin: 1}

func TestManagerCreateGetList(t *testing.T) {
	m := testManager(t, 1)

	a, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.ID() == b.ID() {
		t.Fatalf("two rooms share the id %q", a.ID())
	}
	for _, r := range []*Room{a, b} {
		if !strings.HasPrefix(r.ID(), "r_") {
			t.Errorf("room id %q lacks the r_ prefix", r.ID())
		}
	}

	got, ok := m.Get(a.ID())
	if !ok || got != a {
		t.Errorf("Get(%s) = %v, %v; want the created room", a.ID(), got, ok)
	}
	if _, ok := m.Get("r_nope"); ok {
		t.Error("Get of an unknown id reported success")
	}
	if n := len(m.List()); n != 2 {
		t.Errorf("List() = %d rooms, want 2", n)
	}
}

// TestManagerIsDeterministic pins the project's reproducibility rule at the
// manager level: one process seed fixes both the room ids and the deals inside
// them, so a whole session replays from a single integer.
func TestManagerIsDeterministic(t *testing.T) {
	ids := make([][]string, 2)
	hands := make([][]engine.Card, 2)

	for run := range 2 {
		m := testManager(t, 99)
		for range 3 {
			r, err := m.Create(twoPlayer)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			ids[run] = append(ids[run], r.ID())
		}
		first, _ := m.Get(ids[run][0])
		mustJoin(t, first, "alice")
		hands[run] = mustSnapshot(t, first, "alice").Hand
	}

	for i := range ids[0] {
		if ids[0][i] != ids[1][i] {
			t.Fatalf("room ids differ between runs: %v vs %v", ids[0], ids[1])
		}
	}
	for i := range hands[0] {
		if hands[0][i] != hands[1][i] {
			t.Fatalf("deals differ between runs: %v vs %v", hands[0], hands[1])
		}
	}
}

// TestManagerRoomsHaveIndependentStreams guards against every room dealing the
// same game — the bug a single shared RNG would cause.
func TestManagerRoomsHaveIndependentStreams(t *testing.T) {
	m := testManager(t, 5)

	var first []engine.Card
	for i := range 3 {
		r, err := m.Create(twoPlayer)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		mustJoin(t, r, "alice")
		hand := mustSnapshot(t, r, "alice").Hand
		if i == 0 {
			first = hand
			continue
		}
		same := true
		for j := range hand {
			if hand[j] != first[j] {
				same = false
				break
			}
		}
		if same {
			t.Errorf("room %d was dealt the same hand as room 0: %v", i, hand)
		}
	}
}

func TestManagerCloseRoom(t *testing.T) {
	m := testManager(t, 1)
	r, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.Close(r.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := m.Get(r.ID()); ok {
		t.Error("closed room is still registered")
	}
	if _, err := r.Join(t.Context(), "alice"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("Join on a closed room = %v, want %v", err, ErrRoomClosed)
	}
	if err := m.Close(r.ID()); !errors.Is(err, ErrNoSuchRoom) {
		t.Errorf("second Close = %v, want %v", err, ErrNoSuchRoom)
	}
}

func TestManagerShutdown(t *testing.T) {
	m := testManager(t, 1)
	rooms := make([]*Room, 0, 3)
	for range 3 {
		r, err := m.Create(twoPlayer)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		rooms = append(rooms, r)
	}

	m.Shutdown()
	m.Shutdown() // idempotent

	for _, r := range rooms {
		if _, err := r.Snapshot(t.Context(), ""); !errors.Is(err, ErrRoomClosed) {
			t.Errorf("room %s survived shutdown: %v", r.ID(), err)
		}
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("List() after shutdown = %d, want 0", n)
	}
	if _, err := m.Create(twoPlayer); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Create after shutdown = %v, want %v", err, ErrManagerClosed)
	}
}

func TestManagerCreateRejectsBadOptions(t *testing.T) {
	m := testManager(t, 1)
	if _, err := m.Create(engine.Options{NumPlayers: 5}); err == nil {
		t.Fatal("an unsupported player count should fail at Create, not later")
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("a failed Create registered %d rooms", n)
	}
}

// TestManagerConcurrentAccess exercises the directory lock under -race.
func TestManagerConcurrentAccess(t *testing.T) {
	m := testManager(t, 7)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				r, err := m.Create(twoPlayer)
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				if _, ok := m.Get(r.ID()); !ok {
					t.Errorf("room %s missing right after Create", r.ID())
					return
				}
				m.List()
				if err := m.Close(r.ID()); err != nil {
					t.Errorf("Close: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := len(m.List()); n != 0 {
		t.Errorf("List() = %d rooms, want 0 after every room was closed", n)
	}
}

// TestNewManagerAcceptsNilLogger keeps construction forgiving for callers that
// have not wired logging yet (tests, one-off tools).
func TestNewManagerAcceptsNilLogger(t *testing.T) {
	m := NewManager(nil, func(string) *rand.Rand { return rand.New(rand.NewPCG(1, 2)) })
	t.Cleanup(m.Shutdown)
	if _, err := m.Create(twoPlayer); err != nil {
		t.Fatalf("Create: %v", err)
	}
}
