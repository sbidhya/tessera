package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/store"
	"github.com/sbidhya/tessera/backend/internal/transport"
	"github.com/sbidhya/tessera/backend/internal/wal"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// chooseMove helper copied from room tests for driving a match to finish.
func chooseMoveSnap(snap room.Snapshot) (room.MoveRequest, bool) {
	open := func(c engine.Cell) bool {
		_, occupied := snap.Chips[c]
		return !occupied && !snap.Board.IsCorner(c)
	}
	req := room.MoveRequest{}
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

func playToFinishDirect(t *testing.T, r *room.Room, players []string) {
	t.Helper()
	moves := 0
	for {
		snapTurn, _ := r.Snapshot(t.Context(), "")
		if snapTurn.Status == room.StatusFinished {
			return
		}
		turn := int(snapTurn.Turn)
		player := players[turn]
		snap, _ := r.Snapshot(t.Context(), player)
		req, ok := chooseMoveSnap(snap)
		if !ok {
			t.Fatalf("no move for %s", player)
		}
		req.PlayerID = player
		req.MoveID = "m" + string(rune('0'+moves%10)) + "-" + player
		// Ensure unique
		req.MoveID = "m_" + player + "_" + time.Now().String()
		// Use fmt
		req.MoveID = "m" + string(rune(moves+'a'))
		if _, err := r.PlayMove(t.Context(), req); err != nil {
			t.Fatalf("PlayMove: %v", err)
		}
		moves++
		if moves > 5000 {
			t.Fatal("did not finish")
		}
	}
}

func TestHistoryEndpointServesColdStorage(t *testing.T) {
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

	cfg := config.Config{Seed: 555}
	manager, err := room.NewDurableManager(testLogger(), cfg.NewRand, journal)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	flusher := store.NewFlusher(cold, journal, manager, testLogger())
	// No background ticker needed for this test; use synchronous Flush.

	api := transport.New(manager, testLogger())
	api.SetFlushHook(flusher.Enqueue)

	// Create server mux with cold store mounted
	handler := newRouterWithStore(testLogger(), time.Now(), time.Now, api.Handler(), cold)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Initially empty history
	resp, err := http.Get(srv.URL + "/v1/history")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d, want 200", resp.StatusCode)
	}
	var histResp struct {
		Matches []store.MatchRecord `json:"matches"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&histResp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	resp.Body.Close()
	if len(histResp.Matches) != 0 {
		t.Fatalf("initial history len = %d, want 0", len(histResp.Matches))
	}

	// Create and finish a match
	r, err := manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _ = r.Join(t.Context(), "alice")
	_, _ = r.Join(t.Context(), "bob")

	// Play until finished using deterministic loop
	players := []string{"alice", "bob"}
	moves := 0
	for {
		snap, _ := r.Snapshot(t.Context(), "")
		if snap.Status == room.StatusFinished {
			break
		}
		turnSnap, _ := r.Snapshot(t.Context(), "")
		turnPlayer := players[turnSnap.Turn]
		s, _ := r.Snapshot(t.Context(), turnPlayer)
		req, ok := chooseMoveSnap(s)
		if !ok {
			t.Fatalf("no move for %s", turnPlayer)
		}
		req.PlayerID = turnPlayer
		req.MoveID = "mid" + string(rune('a'+moves))
		if _, err := r.PlayMove(t.Context(), req); err != nil {
			t.Fatalf("PlayMove %d: %v", moves, err)
		}
		moves++
		if moves > 5000 {
			t.Fatal("not finished")
		}
	}

	// Flush to cold
	if err := flusher.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// History should now contain the finished match
	resp, err = http.Get(srv.URL + "/v1/history?limit=10&offset=0")
	if err != nil {
		t.Fatalf("GET history after: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("history after status = %d, body %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&histResp); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	resp.Body.Close()
	if len(histResp.Matches) != 1 || histResp.Matches[0].ID != r.ID() {
		t.Fatalf("history after = %+v, want 1 with id %s", histResp.Matches, r.ID())
	}

	// Stats endpoint
	resp, err = http.Get(srv.URL + "/v1/players/alice/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d", resp.StatusCode)
	}
	var statsResp struct {
		Stats store.PlayerStats `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statsResp); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	resp.Body.Close()
	if statsResp.Stats.GamesPlayed != 1 {
		t.Fatalf("alice stats = %+v, want games 1", statsResp.Stats)
	}

	// WAL checkpointed
	if journal.Exists(r.ID()) {
		t.Fatal("WAL should be checkpointed after flush")
	}
}

func TestHistoryPaginationBadRequest(t *testing.T) {
	storePath := t.TempDir() + "/tessera.db"
	cold, _ := store.Open(storePath)
	t.Cleanup(func() { _ = cold.Close() })
	handler := newRouterWithStore(testLogger(), time.Now(), time.Now, nil, cold)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, _ := http.Get(srv.URL + "/v1/history?limit=-1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit -1 status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
