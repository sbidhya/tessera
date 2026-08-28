package wal_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/wal"
)

const (
	crashHelperEnv = "TESSERA_WAL_CRASH_HELPER"
	crashDirEnv    = "TESSERA_WAL_CRASH_DIR"
)

// TestKilledProcessRecoversMatch is the B4 crash gate. The child process
// creates and advances a match, then calls os.Exit without closing the manager
// or WAL. A fresh process-level manager rebuilds the authoritative state and
// the accepted move-id cache from fsynced records.
func TestKilledProcessRecoversMatch(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWriterProcess$")
	cmd.Env = append(os.Environ(), crashHelperEnv+"=1", crashDirEnv+"="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash writer failed: %v\n%s", err, output)
	}

	store, err := wal.Open(dir, wal.SyncAlways)
	if err != nil {
		t.Fatalf("Open recovered WAL: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Config{Seed: 987654} // deliberately differs from the writer
	manager, err := room.NewDurableManager(quietLogger(), cfg.NewRand, store)
	if err != nil {
		t.Fatalf("recover manager: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	rooms := manager.List()
	if len(rooms) != 1 {
		t.Fatalf("recovered rooms = %d, want 1", len(rooms))
	}
	snap, err := rooms[0].Snapshot(t.Context(), "alice")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Seq != 4 || len(snap.Chips) != 1 || snap.Turn != 1 {
		t.Fatalf("recovered state seq/chips/turn = %d/%d/%d, want 4/1/1", snap.Seq, len(snap.Chips), snap.Turn)
	}
	for _, player := range snap.Players {
		if player.Present {
			t.Errorf("player %s remained present after process death", player.ID)
		}
	}

	retry, err := rooms[0].PlayMove(t.Context(), room.MoveRequest{
		PlayerID: "alice",
		MoveID:   "before-crash",
	})
	if err != nil {
		t.Fatalf("retry recovered move: %v", err)
	}
	if !retry.Duplicate || retry.Seq != 4 {
		t.Errorf("retry = %+v, want duplicate ack at seq 4", retry)
	}
}

func TestCrashWriterProcess(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		return
	}
	dir := os.Getenv(crashDirEnv)
	store, err := wal.Open(dir, wal.SyncAlways)
	crashMust(err)
	cfg := config.Config{Seed: 123}
	manager, err := room.NewDurableManager(quietLogger(), cfg.NewRand, store)
	crashMust(err)
	match, err := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	crashMust(err)
	_, err = match.Join(t.Context(), "alice")
	crashMust(err)
	_, err = match.Join(t.Context(), "bob")
	crashMust(err)
	snap, err := match.Snapshot(t.Context(), "alice")
	crashMust(err)
	move, ok := firstPlacement(snap)
	if !ok {
		panic("crash helper: no legal opening placement")
	}
	move.PlayerID = "alice"
	move.MoveID = "before-crash"
	move.ExpectedSeq = snap.Seq
	_, err = match.PlayMove(t.Context(), move)
	crashMust(err)

	// Intentionally bypass every defer/Close path to model abrupt termination.
	os.Exit(0)
}

func firstPlacement(snap room.Snapshot) (room.MoveRequest, bool) {
	for _, card := range snap.Hand {
		if card.IsOneEyedJack() {
			continue
		}
		if card.IsTwoEyedJack() {
			for row := 0; row < engine.BoardSize; row++ {
				for col := 0; col < engine.BoardSize; col++ {
					cell := engine.Cell{Row: row, Col: col}
					if !snap.Board.IsCorner(cell) {
						return room.MoveRequest{Type: engine.MovePlace, Card: card, Cell: cell}, true
					}
				}
			}
		}
		for _, cell := range snap.Board.CellsFor(card) {
			return room.MoveRequest{Type: engine.MovePlace, Card: card, Cell: cell}, true
		}
	}
	return room.MoveRequest{}, false
}

func crashMust(err error) {
	if err != nil {
		panic(fmt.Sprintf("crash helper: %v", err))
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
