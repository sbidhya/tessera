package room

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// This file drives complete matches through the room's public API only — the
// same surface a real client will use over WebSocket in B3. Nothing here
// reaches into the engine state directly, which is the point: it proves a match
// is playable from snapshots alone.

// chooseMove picks any legal move for the snapshot's viewer, preferring a normal
// placement, then a two-eyed jack, then a one-eyed jack removal, then a
// dead-card swap. It reports ok=false when the viewer has nothing to do.
//
// Iteration order is made deterministic (sorted cells, hand order) so a failing
// game can be replayed exactly from its seed.
func chooseMove(snap Snapshot) (MoveRequest, bool) {
	open := func(c engine.Cell) bool {
		_, occupied := snap.Chips[c]
		return !occupied && !snap.Board.IsCorner(c)
	}
	req := MoveRequest{ExpectedSeq: snap.Seq}

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
			for _, cell := range sortedCells(snap.Chips) {
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

	// Nothing playable: try to trade in a dead card (both its cells occupied).
	for _, card := range snap.Hand {
		if card.IsJack() {
			continue
		}
		cells := snap.Board.CellsFor(card)
		if len(cells) == 0 {
			continue
		}
		if !slices.ContainsFunc(cells, open) {
			req.Type, req.Card = engine.MoveDeadCard, card
			return req, true
		}
	}
	return MoveRequest{}, false
}

func sortedCells(chips map[engine.Cell]engine.Chip) []engine.Cell {
	cells := make([]engine.Cell, 0, len(chips))
	for c := range chips {
		cells = append(cells, c)
	}
	slices.SortFunc(cells, func(a, b engine.Cell) int {
		return cmp.Or(cmp.Compare(a.Row, b.Row), cmp.Compare(a.Col, b.Col))
	})
	return cells
}

// legalMove returns a legal move for the snapshot's viewer, failing the test if
// there is none.
func legalMove(t *testing.T, snap Snapshot) MoveRequest {
	t.Helper()
	req, ok := chooseMove(snap)
	if !ok {
		t.Fatal("no legal move available for the snapshot's viewer")
	}
	// Callers set PlayerID/MoveID/ExpectedSeq themselves.
	req.ExpectedSeq = 0
	return req
}

// TestFullGameInProcess is the B2 gate: two players drive a complete match
// through Join/Snapshot/PlayMove and reach a winner, with every accepted move
// advancing the sequence number by exactly one.
func TestFullGameInProcess(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 7, 42, 1234} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			r, err := New("r_game", testLogger(), testRNG(seed), engine.Options{
				NumPlayers:     2,
				SequencesToWin: 1, // fast games; the 2-sequence path is engine-tested
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(r.Close)

			players := []string{"alice", "bob"}
			for _, p := range players {
				mustJoin(t, r, p)
			}

			var lastSeq uint64
			moves := 0
			for {
				snap := mustSnapshot(t, r, players[r.turnOf(t)])
				if snap.Status == StatusFinished {
					break
				}
				req, ok := chooseMove(snap)
				if !ok {
					t.Fatalf("player %d has no legal move after %d moves", snap.Viewer, moves)
				}
				req.PlayerID = players[snap.Viewer]
				req.MoveID = fmt.Sprintf("m%d", moves)

				res, err := r.PlayMove(t.Context(), req)
				if err != nil {
					t.Fatalf("move %d (%+v): %v", moves, req, err)
				}
				if res.Seq != snap.Seq+1 {
					t.Fatalf("move %d: seq %d -> %d, want +1", moves, snap.Seq, res.Seq)
				}
				lastSeq = res.Seq
				moves++
				if moves > 5000 {
					t.Fatal("match did not terminate in 5000 moves")
				}
			}

			final := mustSnapshot(t, r, "alice")
			if final.Winner != 0 && final.Winner != 1 {
				t.Fatalf("finished match has winner %d, want a real seat", final.Winner)
			}
			if final.Status != StatusFinished {
				t.Fatalf("status = %s, want finished", final.Status)
			}
			if final.Seq != lastSeq {
				t.Errorf("final seq = %d, want %d", final.Seq, lastSeq)
			}
			if final.SequencesWon[final.Winner] < 1 {
				t.Errorf("winner %d has %d sequences", final.Winner, final.SequencesWon[final.Winner])
			}
			t.Logf("seed %d: winner=%d moves=%d seq=%d", seed, final.Winner, moves, final.Seq)

			// The match is over: no further move is accepted, from either player.
			for i, p := range players {
				req := MoveRequest{
					PlayerID: p,
					MoveID:   fmt.Sprintf("post-%d", i),
					Type:     engine.MovePlace,
					Card:     engine.Card{Rank: engine.Jack, Suit: engine.Diamonds},
					Cell:     engine.Cell{Row: 4, Col: 4},
				}
				if _, err := r.PlayMove(t.Context(), req); err == nil {
					t.Errorf("%s moved after the match finished", p)
				} else if !errors.Is(err, engine.ErrGameOver) && !errors.Is(err, engine.ErrCardNotInHand) {
					t.Errorf("%s post-match move err = %v, want a rejection", p, err)
				}
			}
			// Nobody new can join a finished match.
			if _, err := r.Join(t.Context(), "carol"); !errors.Is(err, ErrGameFinished) {
				t.Errorf("join after finish = %v, want %v", err, ErrGameFinished)
			}
		})
	}
}

// TestReconnectMidGameResumes models the B3 reconnect flow end to end at the
// room layer: a player leaves mid-match, rejoins, and finds the state exactly
// where it was — same seat, same hand, same board.
func TestReconnectMidGameResumes(t *testing.T) {
	r := seatedRoom(t, 1)

	// Alice plays once so the state is non-trivial.
	req := legalMove(t, mustSnapshot(t, r, "alice"))
	req.PlayerID, req.MoveID = "alice", "m0"
	if _, err := r.PlayMove(t.Context(), req); err != nil {
		t.Fatalf("alice move: %v", err)
	}
	before := mustSnapshot(t, r, "alice")

	if err := r.Leave(t.Context(), "alice"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if res := mustJoin(t, r, "alice"); !res.Rejoined || res.Seat != before.Viewer {
		t.Fatalf("rejoin = %+v, want seat %d", res, before.Viewer)
	}

	after := mustSnapshot(t, r, "alice")
	if !slices.Equal(after.Hand, before.Hand) {
		t.Errorf("hand after reconnect = %v, want %v", after.Hand, before.Hand)
	}
	if len(after.Chips) != len(before.Chips) {
		t.Errorf("chips after reconnect = %d, want %d", len(after.Chips), len(before.Chips))
	}
	if after.Turn != before.Turn {
		t.Errorf("turn after reconnect = %d, want %d", after.Turn, before.Turn)
	}

	// And the match carries on: bob (whose turn it is) can still play.
	bobReq := legalMove(t, mustSnapshot(t, r, "bob"))
	bobReq.PlayerID, bobReq.MoveID = "bob", "m1"
	if _, err := r.PlayMove(t.Context(), bobReq); err != nil {
		t.Fatalf("bob move after alice's reconnect: %v", err)
	}
}

// turnOf reports which seat is on turn, via a spectator snapshot.
func (r *Room) turnOf(t *testing.T) int {
	t.Helper()
	snap, err := r.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return int(snap.Turn)
}
