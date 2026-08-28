package store_test

import (
	"context"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/store"
)

func finishedSnapshot(id string, winner int, players []string) room.Snapshot {
	return room.Snapshot{
		RoomID:         id,
		Seq:            10,
		Status:         room.StatusFinished,
		NumPlayers:     2,
		SequencesToWin: 1,
		Winner:         engine.PlayerID(winner),
		Players: []room.PlayerInfo{
			{ID: players[0], Seat: 0, Present: true},
			{ID: players[1], Seat: 1, Present: false},
		},
	}
}

func TestSaveFinishedPersistsHistoryAndStats(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	snap := finishedSnapshot("r_hist1", 0, []string{"alice", "bob"})
	if err := s.SaveFinished(ctx, snap); err != nil {
		t.Fatalf("SaveFinished: %v", err)
	}
	hist, err := s.ListHistory(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(hist) != 1 || hist[0].ID != "r_hist1" {
		t.Fatalf("history = %+v, want 1 with id r_hist1", hist)
	}
	if hist[0].Winner == nil || *hist[0].Winner != 0 {
		t.Fatalf("winner = %+v, want 0", hist[0].Winner)
	}
	if len(hist[0].Players) != 2 {
		t.Fatalf("players = %+v, want 2", hist[0].Players)
	}
	statsAlice, _ := s.GetPlayerStats(ctx, "alice")
	if statsAlice.GamesPlayed != 1 || statsAlice.Wins != 1 || statsAlice.Losses != 0 {
		t.Fatalf("alice stats = %+v, want 1/1/0", statsAlice)
	}
	statsBob, _ := s.GetPlayerStats(ctx, "bob")
	if statsBob.GamesPlayed != 1 || statsBob.Wins != 0 || statsBob.Losses != 1 {
		t.Fatalf("bob stats = %+v, want 1/0/1", statsBob)
	}
}

func TestSaveFinishedIdempotent(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	snap := finishedSnapshot("r_dup", 1, []string{"alice", "bob"})
	if err := s.SaveFinished(ctx, snap); err != nil {
		t.Fatalf("first SaveFinished: %v", err)
	}
	if err := s.SaveFinished(ctx, snap); err != nil {
		t.Fatalf("second SaveFinished: %v", err)
	}
	hist, _ := s.ListHistory(ctx, 10, 0)
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1", len(hist))
	}
	stats, _ := s.GetPlayerStats(ctx, "alice")
	if stats.GamesPlayed != 1 {
		t.Fatalf("double count: alice stats = %+v", stats)
	}
	statsBob, _ := s.GetPlayerStats(ctx, "bob")
	if statsBob.GamesPlayed != 1 || statsBob.Wins != 1 {
		t.Fatalf("bob stats after dup = %+v", statsBob)
	}
}

func TestSaveBatchAtomicAndIdempotent(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	snaps := []room.Snapshot{
		finishedSnapshot("r_b1", 0, []string{"alice", "bob"}),
		finishedSnapshot("r_b2", 1, []string{"alice", "bob"}),
	}
	if err := s.SaveBatch(ctx, snaps); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	hist, _ := s.ListHistory(ctx, 10, 0)
	if len(hist) != 2 {
		t.Fatalf("hist len = %d, want 2", len(hist))
	}
	// Second batch with same ids should be idempotent
	if err := s.SaveBatch(ctx, snaps); err != nil {
		t.Fatalf("second batch: %v", err)
	}
	hist2, _ := s.ListHistory(ctx, 10, 0)
	if len(hist2) != 2 {
		t.Fatalf("hist2 len = %d, want 2", len(hist2))
	}
	statsAlice, _ := s.GetPlayerStats(ctx, "alice")
	// alice played 2 games, won 1, lost 1
	if statsAlice.GamesPlayed != 2 || statsAlice.Wins != 1 || statsAlice.Losses != 1 {
		t.Fatalf("alice batch stats = %+v, want 2/1/1", statsAlice)
	}
}

func TestSaveFinishedRejectsNonFinished(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	snap := finishedSnapshot("r_bad", 0, []string{"alice", "bob"})
	snap.Status = room.StatusPlaying
	if err := s.SaveFinished(ctx, snap); err == nil {
		t.Fatal("expected error for non-finished snapshot")
	}
}

func TestListHistoryPagination(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		id := "r_" + string(rune('a'+i))
		snap := finishedSnapshot(id, 0, []string{"alice", "bob"})
		if err := s.SaveFinished(ctx, snap); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	hist, _ := s.ListHistory(ctx, 2, 0)
	if len(hist) != 2 {
		t.Fatalf("limit 2 got %d", len(hist))
	}
	hist2, _ := s.ListHistory(ctx, 2, 2)
	if len(hist2) != 2 {
		t.Fatalf("offset 2 got %d", len(hist2))
	}
	hist3, _ := s.ListHistory(ctx, 10, 0)
	if len(hist3) != 5 {
		t.Fatalf("all got %d", len(hist3))
	}
}

func TestGetPlayerStatsZeroForUnknown(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	stats, err := s.GetPlayerStats(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("GetPlayerStats: %v", err)
	}
	if stats.GamesPlayed != 0 || stats.Wins != 0 {
		t.Fatalf("unknown stats = %+v, want zeros", stats)
	}
}
