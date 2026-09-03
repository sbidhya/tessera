package room

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// TestDrawnMatchFinishesAndIsArchived drives a full deck to exhaustion through
// the public API with wins disabled, then requires the terminal state the leak
// report is about: StatusFinished (not stuck in StatusPlaying), no winner, one
// archive enqueueing the draw, and no further moves accepted.
func TestDrawnMatchFinishesAndIsArchived(t *testing.T) {
	r := newTestRoom(t, 100) // no sequence target is reachable; the deck must exhaust
	archive := &captureArchive{matches: make(chan FinishedMatch, 1)}
	r.archive = archive
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")

	players := []string{"alice", "bob"}
	moves := 0
	for {
		snap := mustSnapshot(t, r, "")
		if snap.Status == StatusFinished {
			break
		}
		view := mustSnapshot(t, r, players[snap.Turn])
		req, ok := chooseMove(view)
		if !ok {
			t.Fatalf("player %d has no legal move after %d moves with status %s",
				view.Viewer, moves, snap.Status)
		}
		req.PlayerID = players[view.Viewer]
		req.MoveID = fmt.Sprintf("draw-%d", moves)
		if _, err := r.PlayMove(t.Context(), req); err != nil {
			t.Fatalf("move %d: %v", moves, err)
		}
		moves++
		if moves > 5000 {
			t.Fatal("drawn match did not terminate in 5000 moves")
		}
	}

	final := mustSnapshot(t, r, "alice")
	if final.Status != StatusFinished {
		t.Fatalf("status = %s, want finished", final.Status)
	}
	if final.Winner != engine.NoPlayer {
		t.Fatalf("drawn match has winner %d, want NoPlayer", final.Winner)
	}
	t.Logf("draw after %d moves at seq %d", moves, final.Seq)

	select {
	case archived := <-archive.matches:
		if archived.RoomID != r.ID() {
			t.Errorf("archived room = %s, want %s", archived.RoomID, r.ID())
		}
		if archived.Winner != engine.NoPlayer {
			t.Errorf("archived winner = %d, want NoPlayer", archived.Winner)
		}
		for _, p := range archived.Players {
			if p.Won {
				t.Errorf("archived player %s marked Won in a draw", p.ID)
			}
		}
		if got := archived.History[len(archived.History)-1].Seq; got != archived.FinishedSeq {
			t.Errorf("archived history ends at %d, want FinishedSeq %d", got, archived.FinishedSeq)
		}
	default:
		t.Fatal("drawn match was never handed to the archive sink: room/WAL would leak")
	}

	// The terminal state is immutable: further moves are rejected, not applied.
	req := MoveRequest{
		PlayerID: "alice",
		MoveID:   "post-draw",
		Type:     engine.MovePlace,
		Card:     engine.Card{Rank: engine.Jack, Suit: engine.Diamonds},
		Cell:     engine.Cell{Row: 4, Col: 4},
	}
	if _, err := r.PlayMove(t.Context(), req); !errors.Is(err, engine.ErrGameOver) {
		t.Errorf("post-draw move err = %v, want %v", err, engine.ErrGameOver)
	}
}
