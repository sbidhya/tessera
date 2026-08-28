package store_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/store"
	"github.com/sbidhya/tessera/backend/internal/wal"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func playToFinish(t *testing.T, r *room.Room, players []string) {
	t.Helper()
	// Copied choose logic from room/game_test.go chooseMove behavior (deterministic).
	// We replicate here to avoid import cycle.
	moves := 0
	for {
		// Determine turn via snapshot
		snap, err := r.Snapshot(t.Context(), players[0])
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.Status == room.StatusFinished {
			return
		}
		// Find whose turn
		snapTurn, err := r.Snapshot(t.Context(), "")
		if err != nil {
			t.Fatalf("Snapshot turn: %v", err)
		}
		turnSeat := int(snapTurn.Turn)
		player := players[turnSeat]
		snapP, err := r.Snapshot(t.Context(), player)
		if err != nil {
			t.Fatalf("Snapshot %s: %v", player, err)
		}
		req, ok := chooseMoveForTest(snapP)
		if !ok {
			t.Fatalf("no legal move for %s after %d moves", player, moves)
		}
		req.PlayerID = player
		req.MoveID = fmt.Sprintf("m%d", moves)
		if _, err := r.PlayMove(t.Context(), req); err != nil {
			t.Fatalf("PlayMove %d %+v: %v", moves, req, err)
		}
		moves++
		if moves > 5000 {
			t.Fatal("match did not finish in 5000 moves")
		}
	}
}

// chooseMoveForTest mirrors room.chooseMove but exported for store tests.
func chooseMoveForTest(snap room.Snapshot) (room.MoveRequest, bool) {
	open := func(c engine.Cell) bool {
		_, occupied := snap.Chips[c]
		return !occupied && !snap.Board.IsCorner(c)
	}
	req := room.MoveRequest{}
	// Prefer placements
	for _, card := range snap.Hand {
		switch {
		case card.IsTwoEyedJack():
			for row := 0; row < engine.BoardSize; row++ {
				for col := 0; col < engine.BoardSize; col++ {
					cell := engine.Cell{Row: row, Col: col}
					if open(cell) {
						req.Type, req.Card, req.Cell = engine.MovePlace, card, cell
						return req, true
					}
				}
			}
		case card.IsOneEyedJack():
			// Find removable
			for cell, chip := range snap.Chips {
				if chip.Owner != snap.Viewer && !chip.InSequence {
					req.Type, req.Card, req.Cell = engine.MoveRemove, card, cell
					return req, true
				}
			}
		default:
			for _, cell := range snap.Board.CellsFor(card) {
				if open(cell) {
					req.Type, req.Card, req.Cell = engine.MovePlace, card, cell
					return req, true
				}
			}
		}
	}
	for _, card := range snap.Hand {
		if card.IsJack() {
			continue
		}
		cells := snap.Board.CellsFor(card)
		dead := true
		for _, c := range cells {
			if open(c) {
				dead = false
				break
			}
		}
		if dead && len(cells) > 0 {
			req.Type, req.Card = engine.MoveDeadCard, card
			return req, true
		}
	}
	return room.MoveRequest{}, false
}

func TestFlusherPersistsFinishedAndCheckpoints(t *testing.T) {
	walDir := t.TempDir()
	storePath := t.TempDir() + "/tessera.db"

	journal, err := wal.Open(walDir, wal.SyncAlways)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })

	cold, err := store.Open(storePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = cold.Close() })

	cfg := config.Config{Seed: 42}
	manager, err := room.NewDurableManager(quietLogger(), cfg.NewRand, journal)
	if err != nil {
		t.Fatalf("NewDurableManager: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	// Create and finish one match.
	r, err := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := r.Join(t.Context(), "alice"); err != nil {
		t.Fatalf("Join alice: %v", err)
	}
	if _, err := r.Join(t.Context(), "bob"); err != nil {
		t.Fatalf("Join bob: %v", err)
	}
	playToFinish(t, r, []string{"alice", "bob"})

	snap, _ := r.Snapshot(t.Context(), "alice")
	if snap.Status != room.StatusFinished {
		t.Fatalf("match not finished after play: %+v", snap)
	}
	if !journal.Exists(r.ID()) {
		t.Fatal("WAL should exist before flush")
	}

	flusher := store.NewFlusher(cold, journal, manager, quietLogger())
	// Synchronous flush (no background ticker needed).
	if err := flusher.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Verify history
	hist, err := cold.ListHistory(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(hist) != 1 || hist[0].ID != r.ID() {
		t.Fatalf("history = %+v, want 1 with id %s", hist, r.ID())
	}
	// WAL checkpointed
	if journal.Exists(r.ID()) {
		t.Fatalf("WAL not checkpointed after flush; file still exists for %s", r.ID())
	}
	// Stats
	statsAlice, _ := cold.GetPlayerStats(context.Background(), "alice")
	if statsAlice.GamesPlayed != 1 {
		t.Fatalf("alice stats = %+v, want games 1", statsAlice)
	}
	// Idempotent second flush
	if err := flusher.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	hist2, _ := cold.ListHistory(context.Background(), 10, 0)
	if len(hist2) != 1 {
		t.Fatalf("idempotent second flush doubled history: %d", len(hist2))
	}
	stats2, _ := cold.GetPlayerStats(context.Background(), "alice")
	if stats2.GamesPlayed != 1 {
		t.Fatalf("double count after second flush: %+v", stats2)
	}
}

func TestFlusherBatchesMultipleMatches(t *testing.T) {
	walDir := t.TempDir()
	storePath := t.TempDir() + "/tessera.db"
	journal, _ := wal.Open(walDir, wal.SyncAlways)
	t.Cleanup(func() { _ = journal.Close() })
	cold, _ := store.Open(storePath)
	t.Cleanup(func() { _ = cold.Close() })
	cfg := config.Config{Seed: 99}
	manager, _ := room.NewDurableManager(quietLogger(), cfg.NewRand, journal)
	t.Cleanup(manager.Shutdown)

	// Create 3 matches and finish each.
	var ids []string
	for i := 0; i < 3; i++ {
		r, _ := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
		_, _ = r.Join(t.Context(), "alice")
		_, _ = r.Join(t.Context(), "bob")
		playToFinish(t, r, []string{"alice", "bob"})
		ids = append(ids, r.ID())
	}
	flusher := store.NewFlusher(cold, journal, manager, quietLogger())
	if err := flusher.Flush(context.Background()); err != nil {
		t.Fatalf("Flush batch: %v", err)
	}
	hist, _ := cold.ListHistory(context.Background(), 10, 0)
	if len(hist) != 3 {
		t.Fatalf("batch history len = %d, want 3", len(hist))
	}
	for _, id := range ids {
		if journal.Exists(id) {
			t.Fatalf("WAL not checkpointed for %s", id)
		}
	}
	stats, _ := cold.GetPlayerStats(context.Background(), "alice")
	if stats.GamesPlayed != 3 {
		t.Fatalf("alice games = %d, want 3", stats.GamesPlayed)
	}
}

func TestCrashBetweenWALAndSQLiteStillRecovers(t *testing.T) {
	walDir := t.TempDir()
	storePath := t.TempDir() + "/tessera.db"

	// First incarnation: create and finish a match, but DO NOT flush to SQLite.
	// Simulate crash by closing manager/wal without flushing.
	var finishedID string
	{
		journal, _ := wal.Open(walDir, wal.SyncAlways)
		cold, _ := store.Open(storePath)
		_ = cold.Close() // not used in this incarnation
		cfg := config.Config{Seed: 123}
		manager, _ := room.NewDurableManager(quietLogger(), cfg.NewRand, journal)
		r, _ := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
		_, _ = r.Join(t.Context(), "alice")
		_, _ = r.Join(t.Context(), "bob")
		playToFinish(t, r, []string{"alice", "bob"})
		finishedID = r.ID()
		snap, _ := r.Snapshot(t.Context(), "alice")
		if snap.Status != room.StatusFinished {
			t.Fatalf("first incarnation not finished")
		}
		if !journal.Exists(finishedID) {
			t.Fatal("WAL missing before crash")
		}
		manager.Shutdown()
		_ = journal.Close()
		// cold store was empty, not flushed
		// Simulate store that was empty: check that before restart, store has 0
		cold2, _ := store.Open(storePath)
		hist, _ := cold2.ListHistory(context.Background(), 10, 0)
		if len(hist) != 0 {
			t.Fatalf("store should be empty before recovery, got %d", len(hist))
		}
		_ = cold2.Close()
	}

	// Second incarnation: replay WAL and flush.
	journal2, _ := wal.Open(walDir, wal.SyncAlways)
	t.Cleanup(func() { _ = journal2.Close() })
	cold2, _ := store.Open(storePath)
	t.Cleanup(func() { _ = cold2.Close() })
	cfg2 := config.Config{Seed: 9999} // different seed must not affect replay
	manager2, err := room.NewDurableManager(quietLogger(), cfg2.NewRand, journal2)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	t.Cleanup(manager2.Shutdown)

	// Verify recovered match is still finished
	r2, ok := manager2.Get(finishedID)
	if !ok {
		t.Fatalf("recovered manager missing room %s", finishedID)
	}
	snap2, _ := r2.Snapshot(t.Context(), "alice")
	if snap2.Status != room.StatusFinished {
		t.Fatalf("recovered status = %s, want finished", snap2.Status.String())
	}

	// Now flush – this is the recovery flush that was missed in first incarnation.
	flusher := store.NewFlusher(cold2, journal2, manager2, quietLogger())
	if err := flusher.Flush(context.Background()); err != nil {
		t.Fatalf("recovery Flush: %v", err)
	}
	hist, _ := cold2.ListHistory(context.Background(), 10, 0)
	if len(hist) != 1 || hist[0].ID != finishedID {
		t.Fatalf("recovery history = %+v, want 1 with id %s", hist, finishedID)
	}
	if journal2.Exists(finishedID) {
		t.Fatal("WAL should be checkpointed after recovery flush")
	}
	// Idempotent: another flush does not duplicate
	if err := flusher.Flush(context.Background()); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	hist2, _ := cold2.ListHistory(context.Background(), 10, 0)
	if len(hist2) != 1 {
		t.Fatalf("double flush after recovery: hist len %d", len(hist2))
	}
}

func TestFlusherAsyncEnqueueAndBackgroundBatch(t *testing.T) {
	walDir := t.TempDir()
	storePath := t.TempDir() + "/tessera.db"
	journal, _ := wal.Open(walDir, wal.SyncAlways)
	t.Cleanup(func() { _ = journal.Close() })
	cold, _ := store.Open(storePath)
	t.Cleanup(func() { _ = cold.Close() })
	cfg := config.Config{Seed: 7}
	manager, _ := room.NewDurableManager(quietLogger(), cfg.NewRand, journal)
	t.Cleanup(manager.Shutdown)

	r, _ := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join(t.Context(), "alice")
	_, _ = r.Join(t.Context(), "bob")
	playToFinish(t, r, []string{"alice", "bob"})

	flusher := store.NewFlusher(cold, journal, manager, quietLogger())
	flusher.Start()
	t.Cleanup(flusher.Stop)
	flusher.Enqueue(r.ID())

	// Wait for the background ticker (100ms) to fire. Poll with 50ms sleeps.
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		hist, _ := cold.ListHistory(context.Background(), 10, 0)
		if len(hist) == 1 {
			break
		}
	}
	hist, _ := cold.ListHistory(context.Background(), 10, 0)
	if len(hist) != 1 {
		t.Fatalf("async flush history len = %d, want 1", len(hist))
	}
	if journal.Exists(r.ID()) {
		t.Fatalf("WAL should be checkpointed after async flush")
	}
}
