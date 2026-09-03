package store

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	_ "modernc.org/sqlite"
)

// openPoisonStore opens a store whose cleanup tolerates Close failing: these
// tests deliberately leave a permanently-rejected queue item behind (the P0-5
// poison-item behaviour), and the shared testStore helper treats any Close
// error as a test failure.
func openPoisonStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir()+"/tessera.db", &checkpointRecorder{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{BatchSize: 8, FlushInterval: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// drawnMatch builds a terminal draw projection: no winner seat and no Won
// player, with a complete event history like finishedMatch.
func drawnMatch(id string) room.FinishedMatch {
	players := []room.FinishedPlayer{
		{ID: "alice", Seat: 0, Sequences: 1},
		{ID: "bob", Seat: 1, Sequences: 0},
	}
	return room.FinishedMatch{
		RoomID:         id,
		FinishedSeq:    4,
		NumPlayers:     2,
		SequencesToWin: 1,
		Winner:         engine.NoPlayer,
		Players:        players,
		History: []room.Event{
			{Version: room.EventVersion, Type: room.EventRoomCreated, RoomID: id, Seq: 1,
				Options: engine.Options{NumPlayers: 2, SequencesToWin: 1}, RNGSeed: [2]uint64{1, 2}},
			{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: id, Seq: 2, PlayerID: "alice"},
			{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: id, Seq: 3, PlayerID: "bob"},
			{Version: room.EventVersion, Type: room.EventMoveApplied, RoomID: id, Seq: 4,
				Move: room.MoveRequest{PlayerID: "alice", MoveID: "last-move"}},
		},
	}
}

// TestPersistsDrawnMatchWithNullWinner is the store half of the draw contract:
// a zero-winner archive must persist (not become a permanently-failing queue
// item), record NULL winner columns, bank no win or loss for either player,
// and still checkpoint the match WAL.
func TestPersistsDrawnMatchWithNullWinner(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	s := testStore(t, checkpoint, 8)
	match := drawnMatch("r_draw")

	s.MatchFinished(match, nil)
	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := s.Match(t.Context(), match.RoomID)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.WinnerID != "" || got.WinnerSeat != engine.NoPlayer || !got.IsDraw() {
		t.Errorf("match = %+v, want empty winner and NoPlayer seat", got)
	}
	if got.MoveCount != 1 || got.FinishedSeq != 4 {
		t.Errorf("match = %+v, want moveCount 1 and seq 4", got)
	}
	assertStats(t, s, "alice", 1, 0, 0, 1)
	assertStats(t, s, "bob", 1, 0, 0, 0)
	if checkpoint.callCount() != 1 {
		t.Errorf("checkpoint calls = %d, want 1 (draws release the WAL too)", checkpoint.callCount())
	}

	// Retrying the same draw is a no-op for stats, like a retried win.
	s.MatchFinished(match, nil)
	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	assertStats(t, s, "alice", 1, 0, 0, 1)
	assertStats(t, s, "bob", 1, 0, 0, 0)
}

// TestDrawValidationRejectsCorruptProjections pins the boundary of "zero
// winners allowed": a draw must have NoPlayer as its seat, and two winners are
// still rejected. Each case gets a fresh store so a failed queue item cannot
// mask the next flush.
func TestDrawValidationRejectsCorruptProjections(t *testing.T) {
	mismatched := drawnMatch("r_draw_mismatch")
	mismatched.Winner = 0 // seat claims a winner no player result backs
	s := openPoisonStore(t)
	s.MatchFinished(mismatched, nil)
	if err := s.Flush(t.Context()); err == nil {
		t.Error("Flush accepted a draw whose winner seat disagrees with its players")
	}

	double := finishedMatch("r_double", "alice")
	double.Players[1].Won = true // now two winners, seat still 0
	s2 := openPoisonStore(t)
	s2.MatchFinished(double, nil)
	if err := s2.Flush(t.Context()); err == nil {
		t.Error("Flush accepted a match with two winners")
	}
}

// TestMigrationRelaxesWinnerColumns replays the upgrade path: a database file
// created with the pre-draw NOT NULL schema keeps its archived win and then
// accepts a draw once Open migrates the columns to nullable.
func TestMigrationRelaxesWinnerColumns(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacy := []string{
		`CREATE TABLE matches (
			id TEXT PRIMARY KEY,
			finished_seq INTEGER NOT NULL,
			num_players INTEGER NOT NULL,
			sequences_to_win INTEGER NOT NULL,
			winner_player_id TEXT NOT NULL,
			winner_seat INTEGER NOT NULL,
			move_count INTEGER NOT NULL,
			archive_hash BLOB NOT NULL,
			archived_at TEXT NOT NULL
		)`,
		`INSERT INTO matches
			(id, finished_seq, num_players, sequences_to_win, winner_player_id, winner_seat, move_count, archive_hash, archived_at)
			VALUES ('r_legacy_win', 4, 2, 1, 'alice', 0, 1, x'00', '2024-01-01T00:00:00Z')`,
	}
	for _, statement := range legacy {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed legacy db: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	checkpoint := &checkpointRecorder{}
	s, err := Open(path, checkpoint, nil, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// The pre-migration win survives the rebuild untouched.
	preserved, err := s.Match(context.Background(), "r_legacy_win")
	if err != nil {
		t.Fatalf("Match(r_legacy_win): %v", err)
	}
	if preserved.WinnerID != "alice" || preserved.WinnerSeat != 0 {
		t.Errorf("legacy match = %+v, want winner alice at seat 0", preserved)
	}

	// And a draw now archives instead of failing on NOT NULL columns.
	draw := drawnMatch("r_post_migration_draw")
	s.MatchFinished(draw, nil)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush draw after migration: %v", err)
	}
	got, err := s.Match(context.Background(), draw.RoomID)
	if err != nil {
		t.Fatalf("Match(draw): %v", err)
	}
	if !got.IsDraw() {
		t.Errorf("migrated match = %+v, want a draw", got)
	}
}
