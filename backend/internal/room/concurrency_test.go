package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// These tests are written for `go test -race`. They exist to prove the two
// claims the actor model makes: that the room's state is touched by exactly one
// goroutine no matter how many callers pile on, and that retries under
// concurrency still apply a move exactly once.

// TestConcurrentDuplicatesApplyOnce fires the identical move request from many
// goroutines at once — the pathological version of a mobile client retrying on
// a flaky link. Exactly one submission may apply; every other must come back as
// a duplicate carrying the same result.
func TestConcurrentDuplicatesApplyOnce(t *testing.T) {
	r := seatedRoom(t, 2)

	req := legalMove(t, mustSnapshot(t, r, "alice"))
	req.PlayerID = "alice"
	req.MoveID = "retry-me"

	const senders = 32
	results := make([]MoveResult, senders)
	errs := make([]error, senders)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together to maximise interleaving
			results[i], errs[i] = r.PlayMove(t.Context(), req)
		}()
	}
	close(start)
	wg.Wait()

	applied := 0
	for i := range senders {
		if errs[i] != nil {
			t.Fatalf("sender %d: %v", i, errs[i])
		}
		if !results[i].Duplicate {
			applied++
		}
		if results[i].Seq != results[0].Seq {
			t.Errorf("sender %d saw seq %d, sender 0 saw %d", i, results[i].Seq, results[0].Seq)
		}
	}
	if applied != 1 {
		t.Errorf("%d submissions applied, want exactly 1", applied)
	}

	snap := mustSnapshot(t, r, "alice")
	if len(snap.Chips) != 1 {
		t.Errorf("chips = %d, want 1", len(snap.Chips))
	}
	if len(snap.Hand) != 7 {
		t.Errorf("hand = %d cards, want 7 (one card spent, one drawn)", len(snap.Hand))
	}
}

// TestConcurrentPlayUnderLoad runs a whole match with both players hammering the
// room concurrently — each retrying blindly, out of turn, while extra goroutines
// read snapshots and churn Leave/Join. Under -race this is the check that no
// state escapes the room goroutine.
//
// It also asserts a correctness property that concurrency could plausibly break:
// each move id is applied at most once, so the number of chips plus removals on
// the board is explained exactly by the accepted moves.
func TestConcurrentPlayUnderLoad(t *testing.T) {
	r, err := New("r_load", testLogger(), testRNG(3), engine.Options{
		NumPlayers:     2,
		SequencesToWin: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)

	players := []string{"alice", "bob"}
	for _, p := range players {
		mustJoin(t, r, p)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var accepted, duplicates atomic.Int64
	// drivers finish on their own; helpers run until the context is cancelled.
	var drivers, helpers sync.WaitGroup

	// One driver per player. Each keeps proposing moves for its own seat; when
	// it is not that player's turn the engine rejects and the driver loops.
	// Every attempt is submitted twice, so half the traffic is a retry.
	for _, name := range players {
		drivers.Add(1)
		go func() {
			defer drivers.Done()
			for n := 0; ctx.Err() == nil && n < 400; n++ {
				snap, err := r.Snapshot(ctx, name)
				if err != nil || snap.Status == StatusFinished {
					cancel()
					return
				}
				req, ok := chooseMove(snap)
				if !ok {
					return
				}
				req.PlayerID = name
				req.MoveID = fmt.Sprintf("%s-%d", name, n)
				req.ExpectedSeq = 0 // races make any expectation stale; rely on the engine

				for attempt := 0; attempt < 2; attempt++ {
					res, err := r.PlayMove(ctx, req)
					switch {
					case err == nil && res.Duplicate:
						duplicates.Add(1)
					case err == nil:
						accepted.Add(1)
					case errors.Is(err, engine.ErrNotYourTurn),
						errors.Is(err, engine.ErrGameOver),
						errors.Is(err, engine.ErrCellOccupied),
						errors.Is(err, engine.ErrNotRemovable),
						errors.Is(err, context.Canceled):
						// Expected under contention: the other player got there
						// first, or the match ended mid-flight.
					default:
						t.Errorf("%s move %d: unexpected error %v", name, n, err)
						cancel()
						return
					}
				}
			}
		}()
	}

	// Readers: concurrent snapshotting must never see torn state or race.
	for i := range 4 {
		helpers.Add(1)
		go func() {
			defer helpers.Done()
			viewer := players[i%len(players)]
			for ctx.Err() == nil {
				snap, err := r.Snapshot(ctx, viewer)
				if err != nil {
					return
				}
				// Touch the copies to make any aliasing a race, not a silent read.
				for cell := range snap.Chips {
					_ = cell
				}
				for _, c := range snap.Hand {
					_ = c
				}
			}
		}()
	}

	// Churn: a player dropping and reconnecting while the match runs.
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		for ctx.Err() == nil {
			if err := r.Leave(ctx, "bob"); err != nil {
				return
			}
			if _, err := r.Join(ctx, "bob"); err != nil {
				return
			}
		}
	}()

	drivers.Wait()
	cancel()
	helpers.Wait()

	if accepted.Load() == 0 {
		t.Fatal("no moves were accepted under load")
	}
	if duplicates.Load() == 0 {
		t.Error("no duplicate retries were detected; the load test is not exercising idempotency")
	}

	snap := mustSnapshot(t, r, "alice")
	if int(snap.Seq) <= 0 {
		t.Fatalf("seq = %d", snap.Seq)
	}
	// Chip count can only be explained by accepted placements; duplicates must
	// have contributed nothing.
	if len(snap.Chips) > int(accepted.Load()) {
		t.Errorf("%d chips on the board from only %d accepted moves",
			len(snap.Chips), accepted.Load())
	}
	t.Logf("accepted=%d duplicates=%d chips=%d status=%s",
		accepted.Load(), duplicates.Load(), len(snap.Chips), snap.Status)
}

// TestCloseUnblocksWaitingCallers checks the shutdown path cannot strand a
// caller: closing a room while calls are in flight must fail them, not hang.
func TestCloseUnblocksWaitingCallers(t *testing.T) {
	r := seatedRoom(t, 2)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, err := r.Snapshot(context.Background(), "alice"); err != nil {
					if !errors.Is(err, ErrRoomClosed) {
						t.Errorf("caller %d: err = %v, want %v", i, err, ErrRoomClosed)
					}
					return
				}
			}
		}()
	}

	r.Close()
	wg.Wait() // hangs (and the test times out) if Close can strand a caller
}
