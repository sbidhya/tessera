package match

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func newTestMatchmaker(t *testing.T, seed int64) (*Matchmaker, *room.Manager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: seed}
	manager := room.NewManager(logger, cfg.NewRand)
	mm := NewMatchmaker(logger, manager)
	t.Cleanup(func() {
		mm.Close()
		manager.Shutdown()
	})
	return mm, manager
}

type joinResult struct {
	result Result
	err    error
}

func joinAsync(ctx context.Context, mm *Matchmaker, req Request) <-chan joinResult {
	ch := make(chan joinResult, 1)
	go func() {
		result, err := mm.Join(ctx, req)
		ch <- joinResult{result: result, err: err}
	}()
	return ch
}

// mustDepth polls until the queue settles at want. Joins are submitted from
// helper goroutines, so the depth is not synchronised with the test's next
// line; polling (with a hard deadline) keeps the tests deterministic without
// reaching into the matchmaker's internals.
func mustDepth(t *testing.T, mm *Matchmaker, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := mm.QueueDepth(ctx)
		cancel()
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue depth = %d, want %d (timed out)", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPairTwoPlayers(t *testing.T) {
	mm, manager := newTestMatchmaker(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice := joinAsync(ctx, mm, Request{PlayerID: "alice"})
	// Let alice's join reach the loop before bob queues, so seat order is
	// deterministic: the earlier waiter gets seat 0.
	mustDepth(t, mm, 1)
	bob := joinAsync(ctx, mm, Request{PlayerID: "bob"})

	a := <-alice
	b := <-bob
	if a.err != nil || b.err != nil {
		t.Fatalf("join errors: alice=%v bob=%v", a.err, b.err)
	}
	if a.result.MatchID == "" || a.result.MatchID != b.result.MatchID {
		t.Fatalf("match ids: alice=%q bob=%q", a.result.MatchID, b.result.MatchID)
	}
	if a.result.Seat != 0 || b.result.Seat != 1 {
		t.Fatalf("seats: alice=%d bob=%d, want 0 and 1", a.result.Seat, b.result.Seat)
	}
	mustDepth(t, mm, 0)

	// Both seats were joined by the matchmaker, so the match is already
	// playing before either client opens a socket.
	r, ok := manager.Get(a.result.MatchID)
	if !ok {
		t.Fatalf("paired room %q not in manager", a.result.MatchID)
	}
	snap, err := r.Snapshot(ctx, "alice")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Status != room.StatusPlaying {
		t.Fatalf("paired room status = %v, want playing", snap.Status)
	}
}

func TestPairRespectsSequencesToWin(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	quick := joinAsync(ctx, mm, Request{PlayerID: "quick", SequencesToWin: 1})
	mustDepth(t, mm, 1)
	full := joinAsync(ctx, mm, Request{PlayerID: "full", SequencesToWin: 2})
	mustDepth(t, mm, 2)

	// A second quick-game seeker must pair with the first, not with the
	// full-game waiter, even though the full waiter queued earlier.
	quick2 := joinAsync(ctx, mm, Request{PlayerID: "quick2", SequencesToWin: 1})
	q1 := <-quick
	q2 := <-quick2
	if q1.err != nil || q2.err != nil {
		t.Fatalf("quick pair errors: %v %v", q1.err, q2.err)
	}
	if q1.result.MatchID != q2.result.MatchID {
		t.Fatalf("quick players paired into different matches: %q vs %q", q1.result.MatchID, q2.result.MatchID)
	}
	mustDepth(t, mm, 1)

	select {
	case r := <-full:
		t.Fatalf("full-game waiter paired early: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// Zero means "default" (2) and pairs with an explicit 2.
	def := joinAsync(ctx, mm, Request{PlayerID: "def"})
	f := <-full
	d := <-def
	if f.err != nil || d.err != nil {
		t.Fatalf("default pair errors: %v %v", f.err, d.err)
	}
	if f.result.MatchID != d.result.MatchID {
		t.Fatalf("default/explicit-2 paired into different matches")
	}
	mustDepth(t, mm, 0)
}

func TestDoubleJoinAttachesToSameWaiter(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first := joinAsync(ctx, mm, Request{PlayerID: "alice"})
	mustDepth(t, mm, 1)

	// The pairing partner is released by a timer, long after the duplicate
	// join below has been processed. Attachment is not observable from outside
	// the loop, so the test synchronises the other way round: the partner
	// cannot pair before the gate opens, and the gate opens far later than
	// any in-memory op takes to be processed. A pathological scheduler stall
	// fails loudly (context deadline), never with a false pass.
	bobGate := make(chan struct{})
	bobResult := make(chan joinResult, 1)
	go func() {
		<-bobGate
		result, err := mm.Join(ctx, Request{PlayerID: "bob"})
		bobResult <- joinResult{result: result, err: err}
	}()
	time.AfterFunc(300*time.Millisecond, func() { close(bobGate) })

	// A retry (client timed out and re-queued) attaches to the existing queue
	// entry instead of creating a second one: both calls share the outcome,
	// including the seat.
	dup, dupErr := mm.Join(ctx, Request{PlayerID: "alice"})
	r1 := <-first
	rb := <-bobResult
	if dupErr != nil || r1.err != nil || rb.err != nil {
		t.Fatalf("join errors: dup=%v first=%v bob=%v", dupErr, r1.err, rb.err)
	}
	if dup.MatchID != r1.result.MatchID || dup.MatchID != rb.result.MatchID {
		t.Fatalf("double join produced divergent matches: %+v %+v %+v", dup, r1.result, rb.result)
	}
	if dup.Seat != 0 || r1.result.Seat != 0 {
		t.Fatalf("duplicate join seats = %d / %d, want 0 and 0 (shared waiter)", dup.Seat, r1.result.Seat)
	}
	mustDepth(t, mm, 0)
}

// TestUnwrapCancelRecoversRecentPairing simulates a Join whose context died in
// the same instant its partner was found. The waiter is gone but the pairing
// is real and remembered: Join must report the match (success), not the
// context error, so no ghost seat is stranded.
func TestUnwrapCancelRecoversRecentPairing(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 11)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := joinAsync(ctx, mm, Request{PlayerID: "a"})
	mustDepth(t, mm, 1)
	b := joinAsync(ctx, mm, Request{PlayerID: "b"})
	ra := <-a
	if ra.err != nil {
		t.Fatalf("join a: %v", ra.err)
	}
	if res := <-b; res.err != nil {
		t.Fatalf("join b: %v", res.err)
	}

	gone, stop := context.WithCancel(context.Background())
	stop() // the caller's context is already dead
	got, err := mm.unwrapCancel(gone, "a")
	if err != nil {
		t.Fatalf("unwrapCancel after pairing: %v", err)
	}
	if got.MatchID != ra.result.MatchID || got.Seat != ra.result.Seat {
		t.Fatalf("recovered pairing = %+v, want %+v", got, ra.result)
	}
	if msg := (errPaired{result: got}).Error(); !strings.Contains(msg, got.MatchID) {
		t.Fatalf("errPaired message %q does not name the match", msg)
	}
}

// TestCancelAfterPairingReportsNotQueued pins the recent-pairing ring: once a
// player is paired they are no longer queued, so an explicit Cancel reports
// false — while Join's own context path would still recover the match through
// the same lookup.
func TestCancelAfterPairingReportsNotQueued(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := joinAsync(ctx, mm, Request{PlayerID: "a"})
	mustDepth(t, mm, 1)
	b := joinAsync(ctx, mm, Request{PlayerID: "b"})
	if res := <-a; res.err != nil {
		t.Fatalf("join a: %v", res.err)
	}
	if res := <-b; res.err != nil {
		t.Fatalf("join b: %v", res.err)
	}
	removed, err := mm.Cancel(ctx, "a")
	if err != nil || removed {
		t.Fatalf("Cancel(paired) = %v, %v; want false, nil", removed, err)
	}
}

func TestContextCancelWithdraws(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 4)
	ctx, cancel := context.WithCancel(context.Background())

	pending := joinAsync(ctx, mm, Request{PlayerID: "alice"})
	mustDepth(t, mm, 1)
	cancel()
	res := <-pending
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("cancelled join err = %v, want context.Canceled", res.err)
	}
	mustDepth(t, mm, 0)
}

func TestExplicitCancel(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pending := joinAsync(ctx, mm, Request{PlayerID: "alice"})
	mustDepth(t, mm, 1)

	removed, err := mm.Cancel(ctx, "alice")
	if err != nil || !removed {
		t.Fatalf("Cancel = %v, %v; want true, nil", removed, err)
	}
	// An explicit leave is an answer, not an interruption: the blocked Join
	// reports ErrLeftQueue so the long-poll can end gracefully.
	if res := <-pending; !errors.Is(res.err, ErrLeftQueue) {
		t.Fatalf("pending join after cancel: err = %v, want ErrLeftQueue", res.err)
	}
	mustDepth(t, mm, 0)

	removed, err = mm.Cancel(ctx, "ghost")
	if err != nil || removed {
		t.Fatalf("Cancel(ghost) = %v, %v; want false, nil", removed, err)
	}
}

func TestJoinValidation(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := mm.Join(ctx, Request{PlayerID: ""}); !errors.Is(err, room.ErrInvalidPlayerID) {
		t.Fatalf("empty player err = %v, want ErrInvalidPlayerID", err)
	}
	if _, err := mm.Join(ctx, Request{PlayerID: "x", SequencesToWin: -1}); !errors.Is(err, ErrInvalidSequencesToWin) {
		t.Fatalf("negative stw err = %v, want ErrInvalidSequencesToWin", err)
	}
	mustDepth(t, mm, 0)
}

func TestCloseUnblocksWaiters(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: 7}
	manager := room.NewManager(logger, cfg.NewRand)
	mm := NewMatchmaker(logger, manager)
	defer manager.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pending := joinAsync(ctx, mm, Request{PlayerID: "alice"})
	mustDepth(t, mm, 1)

	mm.Close()
	if res := <-pending; !errors.Is(res.err, ErrMatchmakerClosed) {
		t.Fatalf("join after close: err = %v, want ErrMatchmakerClosed", res.err)
	}
	if _, err := mm.Join(ctx, Request{PlayerID: "late"}); !errors.Is(err, ErrMatchmakerClosed) {
		t.Fatalf("Join after Close err = %v, want ErrMatchmakerClosed", err)
	}
	// Idempotent: a second Close must not hang or panic.
	mm.Close()
}

func TestPairingFailureReleasesWaiters(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: 8}
	manager := room.NewManager(logger, cfg.NewRand)
	mm := NewMatchmaker(logger, manager)
	t.Cleanup(func() {
		mm.Close()
		manager.Shutdown()
	})

	// A shut-down manager cannot create rooms; both waiters must fail instead
	// of hanging, and the queue must drain.
	manager.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := joinAsync(ctx, mm, Request{PlayerID: "a"})
	mustDepth(t, mm, 1)
	b := joinAsync(ctx, mm, Request{PlayerID: "b"})
	ra, rb := <-a, <-b
	if !errors.Is(ra.err, room.ErrManagerClosed) || !errors.Is(rb.err, room.ErrManagerClosed) {
		t.Fatalf("pairing errors = %v / %v, want ErrManagerClosed", ra.err, rb.err)
	}
	mustDepth(t, mm, 0)
}

// TestConcurrentPairStorm queues many players at once and asserts every player
// is paired exactly once, into disjoint rooms. Run under -race: it exercises
// the join/cancel/pair interleavings that a lock-based queue would get wrong.
func TestConcurrentPairStorm(t *testing.T) {
	mm, _ := newTestMatchmaker(t, 9)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const players = 16
	results := make([]<-chan joinResult, players)
	for i := 0; i < players; i++ {
		results[i] = joinAsync(ctx, mm, Request{PlayerID: string(rune('a' + i))})
	}
	seen := make(map[string]int)
	seats := make(map[string]map[int]bool)
	for _, ch := range results {
		r := <-ch
		if r.err != nil {
			t.Fatalf("storm join: %v", r.err)
		}
		seen[r.result.MatchID]++
		if seats[r.result.MatchID] == nil {
			seats[r.result.MatchID] = make(map[int]bool)
		}
		seats[r.result.MatchID][int(r.result.Seat)] = true
	}
	if len(seen) != players/2 {
		t.Fatalf("storm produced %d matches, want %d", len(seen), players/2)
	}
	for id, n := range seen {
		if n != 2 || len(seats[id]) != 2 {
			t.Fatalf("match %s has %d players / seats %v", id, n, seats[id])
		}
	}
	mustDepth(t, mm, 0)
}

func TestPresenceOnlineOffline(t *testing.T) {
	p := NewPresence()
	if p.Count() != 0 || p.IsOnline("alice") {
		t.Fatal("new presence is not empty")
	}
	p.Online("alice")
	p.Online("bob")
	if !p.IsOnline("alice") || !p.IsOnline("bob") {
		t.Fatal("online players not reported")
	}
	if p.Count() != 2 {
		t.Fatalf("count = %d, want 2", p.Count())
	}
	p.Offline("alice")
	if p.IsOnline("alice") || p.Count() != 1 {
		t.Fatal("offline player still reported")
	}
}

func TestPresenceRefcountsSockets(t *testing.T) {
	p := NewPresence()
	p.Online("alice") // match 1 socket
	p.Online("alice") // match 2 socket
	p.Offline("alice")
	if !p.IsOnline("alice") {
		t.Fatal("player with one remaining socket reported offline")
	}
	p.Offline("alice")
	if p.IsOnline("alice") || p.Count() != 0 {
		t.Fatal("player with no sockets still reported online")
	}
}

func TestPresenceOfflineClampsAtZero(t *testing.T) {
	p := NewPresence()
	p.Offline("ghost") // never online: must not go negative
	p.Online("alice")
	p.Offline("alice")
	p.Offline("alice") // duplicate disconnect
	if p.IsOnline("alice") || p.Count() != 0 {
		t.Fatal("duplicate offline poisoned presence")
	}
}

func TestPresenceNilIsDisabled(t *testing.T) {
	var p *Presence
	p.Online("alice")  // must not panic
	p.Offline("alice") // must not panic
	if p.IsOnline("alice") || p.Count() != 0 {
		t.Fatal("nil presence reported anyone online")
	}
}

func TestPresenceConcurrent(t *testing.T) {
	p := NewPresence()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := string(rune('a' + w))
			for i := 0; i < 100; i++ {
				p.Online(id)
				if !p.IsOnline(id) {
					t.Error("just-onlined player not reported")
					return
				}
				p.Offline(id)
			}
		}(w)
	}
	wg.Wait()
	if p.Count() != 0 {
		t.Fatalf("count after balanced churn = %d, want 0", p.Count())
	}
}
