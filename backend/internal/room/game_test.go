package room

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// submitFunc submits one move and returns the accepted result. Tests swap it to
// change *how* the move reaches the room (once, or fanned out across goroutines)
// without changing how the game is played.
type submitFunc func(t *testing.T, r *Room, req MoveRequest) MoveResult

// submitOnce is the ordinary path: one caller, one move.
func submitOnce(t *testing.T, r *Room, req MoveRequest) MoveResult {
	t.Helper()
	res, err := r.PlayMove(context.Background(), req)
	if err != nil {
		t.Fatalf("PlayMove %s: %v", req.MoveID, err)
	}
	return res
}

// submitConcurrently fans the identical request out across workers goroutines —
// a network retry storm in miniature — and asserts that exactly one of them
// actually applied it.
func submitConcurrently(workers int) submitFunc {
	return func(t *testing.T, r *Room, req MoveRequest) MoveResult {
		t.Helper()
		results := make([]MoveResult, workers)
		errs := make([]error, workers)

		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = r.PlayMove(context.Background(), req)
			}()
		}
		wg.Wait()

		var applied MoveResult
		fresh := 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("concurrent PlayMove %s (worker %d): %v", req.MoveID, i, errs[i])
			}
			if !results[i].Duplicate {
				fresh++
				applied = results[i]
			}
		}
		if fresh != 1 {
			t.Fatalf("move %s applied %d times across %d concurrent retries, want exactly 1",
				req.MoveID, fresh, workers)
		}
		return applied
	}
}

// maxCommands bounds a driven game so a bug that stalls play fails the test
// instead of hanging it.
const maxCommands = 400

// driveGame plays a complete match through the room's public API using the bot,
// and returns the final snapshot. Every decision is made from a Snapshot, so
// this exercises exactly the surface a real client will use in B3.
func driveGame(t *testing.T, r *Room, submit submitFunc) Snapshot {
	t.Helper()
	ctx := context.Background()
	players := map[engine.PlayerID]PlayerID{0: "alice", 1: "bob"}

	s, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if s.Status != StatusPlaying {
		t.Fatalf("status = %v, want playing before driving a game", s.Status)
	}

	commands := 0
	turnSeat, sweptThisTurn := s.Turn, false

	for !s.GameOver() {
		if commands >= maxCommands {
			t.Fatalf("game did not finish within %d commands (seq %d)", maxCommands, s.Seq)
		}
		if s.Turn != turnSeat {
			turnSeat, sweptThisTurn = s.Turn, false
		}

		mv, ok := chooseMove(s, s.Turn)
		if !ok {
			// Nothing playable: try the once-per-turn dead-card swap, which
			// keeps the turn with the same player.
			card, dead := chooseDeadCard(s, s.Turn)
			if !dead || sweptThisTurn {
				t.Fatalf("seat %d is stuck at seq %d: no legal move and no dead-card swap left", s.Turn, s.Seq)
			}
			mv, sweptThisTurn = botMove{Type: engine.MoveDeadCard, Card: card}, true
		}

		commands++
		res := submit(t, r, MoveRequest{
			Player:      players[s.Turn],
			MoveID:      fmt.Sprintf("m%d", commands),
			ExpectedSeq: s.Seq,
			Type:        mv.Type,
			Card:        mv.Card,
			Cell:        mv.Cell,
		})
		if res.Seq != s.Seq+1 {
			t.Fatalf("seq after move %d = %d, want %d", commands, res.Seq, s.Seq+1)
		}
		s = res.State
	}

	if want := uint64(commands + 1); s.Seq != want {
		t.Errorf("final seq = %d, want %d (one per accepted command, plus the deal)", s.Seq, want)
	}
	if s.Status != StatusFinished {
		t.Errorf("status = %v, want finished", s.Status)
	}
	return s
}

// TestFullTwoPlayerGame is the B2 gate: a whole match driven in-process through
// Join → PlayMove → win, with no direct access to the game state.
func TestFullTwoPlayerGame(t *testing.T) {
	for _, seqToWin := range []int{1, 2} {
		t.Run(fmt.Sprintf("sequences_to_win=%d", seqToWin), func(t *testing.T) {
			r := newTestRoom(t, seqToWin)
			seatBoth(t, r)
			final := driveGame(t, r, submitOnce)

			if !final.GameOver() {
				t.Fatal("game should have a winner")
			}
			if final.SequencesWon[final.Winner] < seqToWin {
				t.Errorf("winner %d has %d sequences, want >= %d",
					final.Winner, final.SequencesWon[final.Winner], seqToWin)
			}
			// Chips in a completed sequence must be locked against one-eyed jacks.
			for _, seq := range final.Sequences {
				for _, c := range seq.Cells {
					if final.Board.IsCorner(c) {
						continue
					}
					if chip, ok := final.Chips[c]; !ok || !chip.InSequence {
						t.Errorf("cell %v in a completed sequence is not locked: %+v", c, chip)
					}
				}
			}
			t.Logf("winner=seat %d after %d moves, %d chips on the board",
				final.Winner, final.Seq-1, len(final.Chips))
		})
	}
}

// TestMovesAfterWinRejected checks the room stops accepting play once finished.
func TestMovesAfterWinRejected(t *testing.T) {
	r := newTestRoom(t, 1)
	seatBoth(t, r)
	final := driveGame(t, r, submitOnce)

	loser := PlayerID("alice")
	if final.Winner == 0 {
		loser = "bob"
	}
	_, err := r.PlayMove(context.Background(), MoveRequest{
		Player: loser, MoveID: "after-the-buzzer", Type: engine.MovePlace,
		Card: final.Hands[1-final.Winner][0], Cell: engine.Cell{Row: 5, Col: 5},
	})
	if err == nil {
		t.Fatal("a move after the game ended should be rejected")
	}
}

// TestGameIsDeterministic pins the property B4's write-ahead replay depends on:
// same seed plus same commands produces byte-identical state.
func TestGameIsDeterministic(t *testing.T) {
	play := func() Snapshot {
		r := newTestRoom(t, 2)
		seatBoth(t, r)
		return driveGame(t, r, submitOnce)
	}
	a, b := play(), play()

	if a.Seq != b.Seq || a.Winner != b.Winner || len(a.Chips) != len(b.Chips) {
		t.Fatalf("games diverged: %d/%d moves, winners %d/%d", a.Seq, b.Seq, a.Winner, b.Winner)
	}
	for cell, chip := range a.Chips {
		if other, ok := b.Chips[cell]; !ok || other != chip {
			t.Fatalf("chip at %v differs: %+v vs %+v", cell, chip, other)
		}
	}
	for seat := range a.Hands {
		for i, card := range a.Hands[seat] {
			if b.Hands[seat][i] != card {
				t.Fatalf("seat %d hand differs at %d: %v vs %v", seat, i, card, b.Hands[seat][i])
			}
		}
	}
}

// TestConcurrentRetriesAndReaders is the -race half of the B2 gate: a full game
// where every move is submitted by 8 goroutines at once (duplicate move ids)
// while more goroutines continuously read snapshots. Exactly one apply per move
// id must survive, and no reader may ever observe a torn state.
func TestConcurrentRetriesAndReaders(t *testing.T) {
	r := newTestRoom(t, 1)
	seatBoth(t, r)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, err := r.Snapshot(context.Background())
				if err != nil {
					return
				}
				// Touch the copied collections: if Snapshot ever handed back the
				// room's live maps, the race detector fires here.
				for cell := range s.Chips {
					_ = s.Chips[cell]
				}
				for seat := range s.Hands {
					_ = len(s.Hands[seat])
				}
			}
		}()
	}

	final := driveGame(t, r, submitConcurrently(8))
	close(stop)
	readers.Wait()

	if !final.GameOver() {
		t.Fatal("game should have a winner")
	}
}

// TestManyRoomsInParallel puts the manager under load: independent matches
// playing simultaneously must not interfere, and each room's RNG stream must be
// independent (different rooms deal different games).
func TestManyRoomsInParallel(t *testing.T) {
	m := NewManager(config.Config{Seed: 7}, nil)
	defer m.Close()

	const rooms = 12
	type outcome struct {
		id     string
		winner engine.PlayerID
		moves  uint64
		hand0  engine.Card
	}
	results := make([]outcome, rooms)

	// Parallel subtests rather than bare goroutines: t.Fatalf inside driveGame
	// is only valid on the goroutine running its own test.
	t.Run("play", func(t *testing.T) {
		for i := range rooms {
			t.Run(fmt.Sprintf("match-%d", i), func(t *testing.T) {
				t.Parallel()
				r, err := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				seatBoth(t, r)
				final := driveGame(t, r, submitOnce)
				results[i] = outcome{r.ID(), final.Winner, final.Seq - 1, final.Hands[0][0]}
			})
		}
	})

	if m.Len() != rooms {
		t.Errorf("manager holds %d rooms, want %d", m.Len(), rooms)
	}
	ids := make(map[string]bool, rooms)
	distinctDeals := make(map[engine.Card]bool)
	for _, o := range results {
		if o.id == "" {
			t.Fatal("a room failed to play out")
		}
		if ids[o.id] {
			t.Fatalf("duplicate room id %s", o.id)
		}
		ids[o.id] = true
		distinctDeals[o.hand0] = true
	}
	// Per-room RNG streams are derived from (seed, room id), so the rooms must
	// not all deal the same first card.
	if len(distinctDeals) < 2 {
		t.Error("every room dealt the same first card — per-room RNG streams are not independent")
	}
}
