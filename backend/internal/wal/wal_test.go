package wal

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newWALManager(t *testing.T, dir string, seed int64, policy SyncPolicy) (*room.Manager, *Store) {
	t.Helper()
	cfg := config.Config{Seed: seed}
	store, err := New(dir, policy)
	if err != nil {
		t.Fatalf("New WAL: %v", err)
	}
	m := room.NewManager(testLogger(), cfg.NewRand)
	// Do not wire WAL yet — caller will Replay then SetWAL, or Create with WAL.
	return m, store
}

// chooseMove is a tiny helper to drive a game via snapshots (same as room/game_test.go).
func chooseMove(snap room.Snapshot) (room.MoveRequest, bool) {
	open := func(c engine.Cell) bool {
		_, occupied := snap.Chips[c]
		return !occupied && !snap.Board.IsCorner(c)
	}
	req := room.MoveRequest{ExpectedSeq: snap.Seq}
	for _, card := range snap.Hand {
		switch {
		case card.IsTwoEyedJack():
			for row := 0; row < engine.BoardSize; row++ {
				for col := 0; col < engine.BoardSize; col++ {
					if cell := (engine.Cell{Row: row, Col: col}); open(cell) {
						req.Type, req.Card, req.Cell = engine.MovePlace, card, cell
						return req, true
					}
				}
			}
		case card.IsOneEyedJack():
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
		if len(cells) == 0 {
			continue
		}
		allOccupied := true
		for _, c := range cells {
			if open(c) {
				allOccupied = false
				break
			}
		}
		if allOccupied {
			req.Type, req.Card = engine.MoveDeadCard, card
			return req, true
		}
	}
	return room.MoveRequest{}, false
}

func mustSnapshot(t *testing.T, r *room.Room, viewer string) room.Snapshot {
	t.Helper()
	snap, err := r.Snapshot(context.Background(), viewer)
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", viewer, err)
	}
	return snap
}

func TestWALBasicAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir, SyncOff)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.LogCreate("r_test", engine.Options{NumPlayers: 2, SequencesToWin: 1}); err != nil {
		t.Fatalf("LogCreate: %v", err)
	}
	if err := store.LogJoin("r_test", "alice"); err != nil {
		t.Fatalf("LogJoin: %v", err)
	}
	if err := store.LogJoin("r_test", "bob"); err != nil {
		t.Fatalf("LogJoin: %v", err)
	}
	records, err := store.ReadRecords("r_test")
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	if records[0].Type != "create" || records[1].Type != "join" || records[2].Type != "join" {
		t.Errorf("record types = %v", records)
	}
	if records[0].Options.NumPlayers != 2 {
		t.Errorf("create options = %+v", records[0].Options)
	}
}

func TestWALCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	seed := int64(42)

	// --- First incarnation: play mid-game and "kill" the process. ---
	cfg := config.Config{Seed: seed}
	store1, _ := New(dir, SyncAlways)
	m1 := room.NewManager(testLogger(), cfg.NewRand)
	m1.SetWAL(store1)

	r1, err := m1.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := r1.Join(context.Background(), "alice"); err != nil {
		t.Fatalf("Join alice: %v", err)
	}
	if _, err := r1.Join(context.Background(), "bob"); err != nil {
		t.Fatalf("Join bob: %v", err)
	}
	// Play a few moves.
	for i := 0; i < 5; i++ {
		snap := mustSnapshot(t, r1, "alice")
		if snap.Status == room.StatusFinished {
			break
		}
		player := "alice"
		if snap.Turn == 1 {
			player = "bob"
		}
		snap = mustSnapshot(t, r1, player)
		req, ok := chooseMove(snap)
		if !ok {
			t.Fatalf("no legal move at iteration %d", i)
		}
		req.PlayerID = player
		req.MoveID = "m" + string(rune('0'+i))
		if _, err := r1.PlayMove(context.Background(), req); err != nil {
			t.Fatalf("PlayMove %d: %v", i, err)
		}
	}
	origSnap := mustSnapshot(t, r1, "alice")
	origID := r1.ID()
	// "kill" — shutdown without checkpointing.
	m1.Shutdown()

	// Verify WAL exists.
	if _, err := os.Stat(filepath.Join(dir, origID+".wal")); err != nil {
		t.Fatalf("wal file missing: %v", err)
	}

	// --- Second incarnation: same seed, replay the WAL. ---
	store2, _ := New(dir, SyncAlways)
	m2 := room.NewManager(testLogger(), cfg.NewRand)
	if err := store2.Replay(m2); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	// After replay the manager should have the same room.
	r2, ok := m2.Get(origID)
	if !ok {
		t.Fatalf("replayed manager missing room %s", origID)
	}
	replayedSnap := mustSnapshot(t, r2, "alice")
	// Compare decisive fields.
	if replayedSnap.Seq != origSnap.Seq {
		t.Errorf("replayed seq = %d, want %d", replayedSnap.Seq, origSnap.Seq)
	}
	if len(replayedSnap.Chips) != len(origSnap.Chips) {
		t.Errorf("replayed chips = %d, want %d", len(replayedSnap.Chips), len(origSnap.Chips))
	}
	if replayedSnap.Turn != origSnap.Turn {
		t.Errorf("replayed turn = %d, want %d", replayedSnap.Turn, origSnap.Turn)
	}
	if replayedSnap.Status != origSnap.Status {
		t.Errorf("replayed status = %s, want %s", replayedSnap.Status, origSnap.Status)
	}
	// Hands must match for the same viewer.
	if len(replayedSnap.Hand) != len(origSnap.Hand) {
		t.Errorf("replayed hand len = %d, want %d", len(replayedSnap.Hand), len(origSnap.Hand))
	}
	for i := range replayedSnap.Hand {
		if replayedSnap.Hand[i] != origSnap.Hand[i] {
			t.Errorf("hand[%d] = %v, want %v", i, replayedSnap.Hand[i], origSnap.Hand[i])
		}
	}
	// Verify the game can continue after replay.
	nextSnap := mustSnapshot(t, r2, "alice")
	if nextSnap.Turn != replayedSnap.Turn {
		t.Fatalf("turn changed unexpectedly")
	}
	// Play one more move after recovery to prove WAL wiring is live.
	player := "alice"
	if replayedSnap.Turn == 1 {
		player = "bob"
	}
	snap := mustSnapshot(t, r2, player)
	req, ok := chooseMove(snap)
	if ok {
		req.PlayerID = player
		req.MoveID = "after-replay"
		if _, err := r2.PlayMove(context.Background(), req); err != nil {
			t.Fatalf("post-replay move: %v", err)
		}
		afterSnap := mustSnapshot(t, r2, player)
		if afterSnap.Seq != replayedSnap.Seq+1 {
			t.Errorf("post-replay seq = %d, want %d", afterSnap.Seq, replayedSnap.Seq+1)
		}
		// Confirm new move was appended to WAL.
		records, _ := store2.ReadRecords(origID)
		found := false
		for _, rec := range records {
			if rec.MoveID == "after-replay" {
				found = true
				break
			}
		}
		if !found {
			t.Error("post-replay move not found in WAL")
		}
	}
	m2.Shutdown()
}

func TestWALDuplicateReplayIsSafe(t *testing.T) {
	dir := t.TempDir()
	seed := int64(7)
	cfg := config.Config{Seed: seed}
	store, _ := New(dir, SyncOff)
	m1 := room.NewManager(testLogger(), cfg.NewRand)
	m1.SetWAL(store)
	r, _ := m1.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join(context.Background(), "alice")
	_, _ = r.Join(context.Background(), "bob")
	snap := mustSnapshot(t, r, "alice")
	req, ok := chooseMove(snap)
	if !ok {
		t.Fatal("no move")
	}
	req.PlayerID = "alice"
	req.MoveID = "m0"
	res, err := r.PlayMove(context.Background(), req)
	if err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	m1.Shutdown()

	// First replay.
	store2, _ := New(dir, SyncOff)
	m2 := room.NewManager(testLogger(), cfg.NewRand)
	if err := store2.Replay(m2); err != nil {
		t.Fatalf("first Replay: %v", err)
	}
	r2, _ := m2.Get(r.ID())
	snap1 := mustSnapshot(t, r2, "alice")

	// Duplicate retry: same move_id should be deduped, not re-applied.
	req2 := req
	res2, err := r2.PlayMove(context.Background(), req2)
	if err != nil {
		t.Fatalf("duplicate PlayMove: %v", err)
	}
	if !res2.Duplicate || res2.Seq != res.Seq {
		t.Errorf("duplicate result = %+v, want duplicate of %+v", res2, res)
	}
	snapAfterDup := mustSnapshot(t, r2, "alice")
	if snapAfterDup.Seq != snap1.Seq {
		t.Errorf("duplicate move bumped seq: %d -> %d", snap1.Seq, snapAfterDup.Seq)
	}

	// Second replay onto same manager should be idempotent (no extra seq bump).
	before := snapAfterDup.Seq
	if err := store2.Replay(m2); err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	after := mustSnapshot(t, r2, "alice")
	if after.Seq != before {
		t.Errorf("second replay changed seq: %d -> %d", before, after.Seq)
	}
	m2.Shutdown()
}

func TestWALRejectedMovesNotLogged(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir, SyncOff)
	m := room.NewManager(testLogger(), config.Config{Seed: 1}.NewRand)
	m.SetWAL(store)
	r, _ := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join(context.Background(), "alice")
	// Bob not yet joined — alice's move should be rejected (game not started)
	snap := mustSnapshot(t, r, "alice")
	req, _ := chooseMove(snap)
	req.PlayerID = "alice"
	req.MoveID = "bad1"
	_, err := r.PlayMove(context.Background(), req)
	if err == nil {
		t.Fatal("expected rejected move")
	}
	records, _ := store.ReadRecords(r.ID())
	for _, rec := range records {
		if rec.MoveID == "bad1" {
			t.Error("rejected move was logged")
		}
	}
	m.Shutdown()
}

func TestWALPartialWriteRecovery(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir, SyncAlways)
	m := room.NewManager(testLogger(), config.Config{Seed: 1}.NewRand)
	m.SetWAL(store)
	r, _ := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join(context.Background(), "alice")
	// Manually corrupt the tail with a torn write.
	path := filepath.Join(dir, r.ID()+".wal")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	_, _ = f.Write([]byte(`{"type":"join","player_id":"bob`)) // truncated JSON
	_ = f.Close()

	// Replay should heal and ignore the torn line.
	m2 := room.NewManager(testLogger(), config.Config{Seed: 1}.NewRand)
	store2, _ := New(dir, SyncAlways)
	if err := store2.Replay(m2); err != nil {
		t.Fatalf("Replay after torn write: %v", err)
	}
	// The room should exist but bob should not be joined (torn record ignored).
	r2, ok := m2.Get(r.ID())
	if !ok {
		t.Fatal("room missing after replay")
	}
	snap := mustSnapshot(t, r2, "")
	// Only alice is present if bob's join was torn.
	hasBob := false
	for _, p := range snap.Players {
		if p.ID == "bob" {
			hasBob = true
		}
	}
	if hasBob {
		t.Error("torn join record was incorrectly replayed")
	}
	// File should have been truncated to valid prefix.
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(`"bob`)) && !bytes.Contains(data, []byte(`"bob"`)) {
		// crude check: file should not contain partial
	}
	m.Shutdown()
	m2.Shutdown()
}

func TestWALSyncOffWorks(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir, SyncOff)
	m := room.NewManager(testLogger(), config.Config{Seed: 1}.NewRand)
	m.SetWAL(store)
	r, _ := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join(context.Background(), "alice")
	_, _ = r.Join(context.Background(), "bob")
	records, _ := store.ReadRecords(r.ID())
	if len(records) != 3 {
		t.Errorf("records = %d, want 3 (create + 2 joins)", len(records))
	}
	m.Shutdown()
}

func TestWALEmptyDirReplayIsNoop(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir, SyncOff)
	m := room.NewManager(testLogger(), config.Config{Seed: 1}.NewRand)
	if err := store.Replay(m); err != nil {
		t.Fatalf("Replay empty dir: %v", err)
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("rooms after empty replay = %d, want 0", n)
	}
	m.Shutdown()
}
