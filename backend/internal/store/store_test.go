package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

type checkpointRecorder struct {
	mu       sync.Mutex
	calls    []checkpointCall
	failures int
	check    func(string, uint64) error
	notified chan checkpointCall
}

type checkpointCall struct {
	roomID string
	seq    uint64
}

func (c *checkpointRecorder) Checkpoint(roomID string, seq uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, checkpointCall{roomID: roomID, seq: seq})
	if c.notified != nil {
		c.notified <- checkpointCall{roomID: roomID, seq: seq}
	}
	if c.check != nil {
		if err := c.check(roomID, seq); err != nil {
			return err
		}
	}
	if c.failures > 0 {
		c.failures--
		return errors.New("injected checkpoint failure")
	}
	return nil
}

func (c *checkpointRecorder) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func testStore(t *testing.T, checkpoint Checkpointer, batch int) *Store {
	t.Helper()
	s, err := Open(t.TempDir()+"/tessera.db", checkpoint,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{BatchSize: batch, FlushInterval: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func finishedMatch(id, winner string) room.FinishedMatch {
	winnerSeat := engine.PlayerID(0)
	if winner == "bob" {
		winnerSeat = 1
	}
	players := []room.FinishedPlayer{
		{ID: "alice", Seat: 0, Sequences: 1, Won: winner == "alice"},
		{ID: "bob", Seat: 1, Sequences: 1, Won: winner == "bob"},
	}
	return room.FinishedMatch{
		RoomID:         id,
		FinishedSeq:    4,
		NumPlayers:     2,
		SequencesToWin: 1,
		Winner:         winnerSeat,
		Players:        players,
		History: []room.Event{
			{Version: room.EventVersion, Type: room.EventRoomCreated, RoomID: id, Seq: 1,
				Options: engine.Options{NumPlayers: 2, SequencesToWin: 1}, RNGSeed: [2]uint64{1, 2}},
			{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: id, Seq: 2, PlayerID: "alice"},
			{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: id, Seq: 3, PlayerID: "bob"},
			{Version: room.EventVersion, Type: room.EventMoveApplied, RoomID: id, Seq: 4,
				Move: room.MoveRequest{PlayerID: winner, MoveID: "winning-move"}},
		},
	}
}

func TestPersistsMatchHistoryAndStatsBeforeCheckpoint(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	s := testStore(t, checkpoint, 8)
	match := finishedMatch("r_history", "alice")

	// The check runs inside Checkpoint itself, proving the row is queryable only
	// after SQLite's transaction has committed.
	checkpoint.check = func(roomID string, seq uint64) error {
		got, err := s.Match(context.Background(), roomID)
		if err != nil {
			return err
		}
		if got.FinishedSeq != seq {
			return errors.New("SQLite row has the wrong terminal sequence")
		}
		return nil
	}

	s.MatchFinished(match, nil)
	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := s.Match(t.Context(), match.RoomID)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.WinnerID != "alice" || got.WinnerSeat != 0 || got.MoveCount != 1 || got.FinishedSeq != 4 {
		t.Errorf("match = %+v", got)
	}
	history, err := s.History(t.Context(), match.RoomID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !reflect.DeepEqual(history, match.History) {
		t.Errorf("history = %+v, want %+v", history, match.History)
	}
	assertStats(t, s, "alice", 1, 1, 0, 1)
	assertStats(t, s, "bob", 1, 0, 1, 1)
	if checkpoint.callCount() != 1 {
		t.Errorf("checkpoint calls = %d, want 1", checkpoint.callCount())
	}
	for id, want := range map[string]bool{match.RoomID: true, "r_missing": false} {
		found, err := s.HasMatch(t.Context(), id)
		if err != nil {
			t.Fatalf("HasMatch(%s): %v", id, err)
		}
		if found != want {
			t.Errorf("HasMatch(%s) = %t, want %t", id, found, want)
		}
	}
}

func TestDatabaseFileIsPrivate(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	path := t.TempDir() + "/tessera.db"
	s, err := Open(path, checkpoint, nil, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCheckpointRetryDoesNotDoubleCountStats(t *testing.T) {
	checkpoint := &checkpointRecorder{failures: 1}
	s := testStore(t, checkpoint, 8)
	match := finishedMatch("r_retry", "bob")
	s.MatchFinished(match, nil)

	if err := s.Flush(t.Context()); err == nil {
		t.Fatal("first Flush succeeded despite checkpoint failure")
	}
	// SQLite committed before the injected WAL failure.
	assertStats(t, s, "alice", 1, 0, 1, 1)
	assertStats(t, s, "bob", 1, 1, 0, 1)

	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	// The match primary key makes the retried transaction a no-op for stats.
	assertStats(t, s, "alice", 1, 0, 1, 1)
	assertStats(t, s, "bob", 1, 1, 0, 1)
	if checkpoint.callCount() != 2 {
		t.Errorf("checkpoint calls = %d, want 2", checkpoint.callCount())
	}
}

func TestArchiveAcknowledgedAfterCommitDespiteCheckpointFailure(t *testing.T) {
	checkpoint := &checkpointRecorder{failures: 1}
	s := testStore(t, checkpoint, 8)
	acknowledged := make(chan struct{})
	s.MatchFinished(finishedMatch("r_ack", "alice"), func() { close(acknowledged) })

	if err := s.Flush(t.Context()); err == nil {
		t.Fatal("first Flush succeeded despite checkpoint failure")
	}
	select {
	case <-acknowledged:
	default:
		t.Fatal("committed archive was not acknowledged after its WAL checkpoint failed")
	}

	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
}

func TestBatchAggregatesPlayerStats(t *testing.T) {
	checkpoint := &checkpointRecorder{notified: make(chan checkpointCall, 2)}
	s := testStore(t, checkpoint, 2)
	s.MatchFinished(finishedMatch("r_batch_a", "alice"), nil)
	s.MatchFinished(finishedMatch("r_batch_b", "bob"), nil)
	for range 2 {
		select {
		case <-checkpoint.notified:
		case <-time.After(2 * time.Second):
			t.Fatal("full batch was not flushed asynchronously")
		}
	}
	assertStats(t, s, "alice", 2, 1, 1, 2)
	assertStats(t, s, "bob", 2, 1, 1, 2)
}

func TestPartialBatchFlushesOnInterval(t *testing.T) {
	checkpoint := &checkpointRecorder{notified: make(chan checkpointCall, 1)}
	s, err := Open(t.TempDir()+"/tessera.db", checkpoint,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{BatchSize: 8, FlushInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.MatchFinished(finishedMatch("r_interval", "alice"), nil)
	select {
	case <-checkpoint.notified:
	case <-time.After(2 * time.Second):
		t.Fatal("partial batch was not flushed on its interval")
	}
	assertStats(t, s, "alice", 1, 1, 0, 1)
}

func TestArchiveIsIdempotent(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	s := testStore(t, checkpoint, 8)
	match := finishedMatch("r_duplicate", "alice")
	for range 2 {
		s.MatchFinished(match, nil)
		if err := s.Flush(t.Context()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	assertStats(t, s, "alice", 1, 1, 0, 1)
	assertStats(t, s, "bob", 1, 0, 1, 1)
}

func TestOpenValidationAndClosedFlush(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	if _, err := Open("", checkpoint, nil, Options{}); err == nil {
		t.Error("Open accepted an empty path")
	}
	if _, err := Open(":memory:", nil, nil, Options{}); err == nil {
		t.Error("Open accepted a nil checkpointer")
	}
	if _, err := Open(":memory:", checkpoint, nil, Options{BatchSize: -1}); err == nil {
		t.Error("Open accepted a negative batch size")
	}

	s, err := Open(":memory:", checkpoint, nil, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Flush(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close = %v, want ErrClosed", err)
	}
}

func assertStats(t *testing.T, s *Store, player string, played, wins, losses, sequences int) {
	t.Helper()
	got, err := s.Stats(t.Context(), player)
	if err != nil {
		t.Fatalf("Stats(%s): %v", player, err)
	}
	if got.MatchesPlayed != played || got.Wins != wins || got.Losses != losses || got.SequencesCompleted != sequences {
		t.Errorf("Stats(%s) = %+v, want played/wins/losses/sequences %d/%d/%d/%d",
			player, got, played, wins, losses, sequences)
	}
}
