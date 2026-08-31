package store_test

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/store"
	"github.com/sbidhya/tessera/backend/internal/wal"
)

const (
	coldCrashHelperEnv = "TESSERA_STORE_CRASH_HELPER"
	coldCrashDirEnv    = "TESSERA_STORE_CRASH_DIR"
)

// TestCrashBetweenWALAndSQLiteRecovers is the B5 crash gate. The child fsyncs a
// complete game to the WAL and exits without ever opening SQLite. On restart,
// room replay reconstructs the terminal result, the write-behind worker stores
// it exactly once, and only then is the per-match WAL emptied.
func TestCrashBetweenWALAndSQLiteRecovers(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreCrashWriterProcess$")
	cmd.Env = append(os.Environ(), coldCrashHelperEnv+"=1", coldCrashDirEnv+"="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash writer failed: %v\n%s", err, output)
	}

	walDir := filepath.Join(dir, "wal")
	journal, err := wal.Open(walDir, wal.SyncAlways)
	if err != nil {
		t.Fatalf("open recovered WAL: %v", err)
	}
	cold, err := store.Open(filepath.Join(dir, "tessera.db"), journal, quietLogger(),
		store.Options{BatchSize: 8, FlushInterval: time.Hour})
	if err != nil {
		_ = journal.Close()
		t.Fatalf("open SQLite: %v", err)
	}
	cfg := config.Config{Seed: 9999} // recovery must use the seed in the WAL
	manager, err := room.NewPersistentManager(quietLogger(), cfg.NewRand, journal, cold)
	if err != nil {
		_ = cold.Close()
		_ = journal.Close()
		t.Fatalf("recover manager: %v", err)
	}

	rooms := manager.List()
	if len(rooms) != 1 {
		t.Fatalf("recovered rooms = %d, want 1", len(rooms))
	}
	matchID := rooms[0].ID()
	snap, err := rooms[0].Snapshot(t.Context(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Status != room.StatusFinished {
		t.Fatalf("recovered status = %s, want finished", snap.Status)
	}

	if err := cold.Flush(t.Context()); err != nil {
		t.Fatalf("flush recovered finish: %v", err)
	}
	record, err := cold.Match(t.Context(), matchID)
	if err != nil {
		t.Fatalf("persisted Match: %v", err)
	}
	if record.FinishedSeq != snap.Seq || record.WinnerSeat != snap.Winner {
		t.Errorf("persisted match = %+v, recovered seq/winner = %d/%d", record, snap.Seq, snap.Winner)
	}
	history, err := cold.History(t.Context(), matchID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) == 0 || history[len(history)-1].Seq != snap.Seq {
		t.Errorf("history ends at %+v, want seq %d", history, snap.Seq)
	}
	info, err := os.Stat(filepath.Join(walDir, matchID+".wal"))
	if err != nil {
		t.Fatalf("stat checkpointed WAL: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("checkpointed WAL size = %d, want 0", info.Size())
	}

	manager.Shutdown()
	if err := cold.Close(); err != nil {
		t.Fatalf("close SQLite: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	// A second restart has no live room to replay—the result now lives only in
	// the cold tier—and the archived stats remain exactly once.
	reopened, err := wal.Open(walDir, wal.SyncAlways)
	if err != nil {
		t.Fatalf("reopen WAL: %v", err)
	}
	defer reopened.Close()
	clean, err := room.NewDurableManager(quietLogger(), cfg.NewRand, reopened)
	if err != nil {
		t.Fatalf("manager after checkpoint: %v", err)
	}
	defer clean.Shutdown()
	if got := len(clean.List()); got != 0 {
		t.Errorf("rooms after checkpoint = %d, want 0", got)
	}
}

func TestStoreCrashWriterProcess(t *testing.T) {
	if os.Getenv(coldCrashHelperEnv) != "1" {
		return
	}
	dir := os.Getenv(coldCrashDirEnv)
	journal, err := wal.Open(filepath.Join(dir, "wal"), wal.SyncAlways)
	crashMust(err)
	cfg := config.Config{Seed: 71}
	manager, err := room.NewDurableManager(quietLogger(), cfg.NewRand, journal)
	crashMust(err)
	match, err := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	crashMust(err)
	_, err = match.Join(t.Context(), "alice")
	crashMust(err)
	_, err = match.Join(t.Context(), "bob")
	crashMust(err)

	players := []string{"alice", "bob"}
	for move := 0; move < 5000; move++ {
		spectator, err := match.Snapshot(t.Context(), "")
		crashMust(err)
		if spectator.Status == room.StatusFinished {
			os.Exit(0) // intentionally skip every Close path
		}
		player := players[spectator.Turn]
		snap, err := match.Snapshot(t.Context(), player)
		crashMust(err)
		req, ok := chooseMove(snap)
		if !ok {
			panic(fmt.Sprintf("crash helper: no legal move after %d plays", move))
		}
		req.PlayerID = player
		req.MoveID = fmt.Sprintf("move-%d", move)
		_, err = match.PlayMove(t.Context(), req)
		crashMust(err)
	}
	panic("crash helper: match did not finish")
}

func chooseMove(snap room.Snapshot) (room.MoveRequest, bool) {
	open := func(cell engine.Cell) bool {
		_, occupied := snap.Chips[cell]
		return !occupied && !snap.Board.IsCorner(cell)
	}
	req := room.MoveRequest{ExpectedSeq: snap.Seq}
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
			cells := make([]engine.Cell, 0, len(snap.Chips))
			for cell := range snap.Chips {
				cells = append(cells, cell)
			}
			slices.SortFunc(cells, func(a, b engine.Cell) int {
				return cmp.Or(cmp.Compare(a.Row, b.Row), cmp.Compare(a.Col, b.Col))
			})
			for _, cell := range cells {
				chip := snap.Chips[cell]
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
		if len(cells) > 0 && !slices.ContainsFunc(cells, open) {
			req.Type, req.Card = engine.MoveDeadCard, card
			return req, true
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
