package room

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// testRand returns the same deterministic stream on every call, so a test that
// fails can be reproduced exactly.
func testRand() *rand.Rand {
	return config.Config{Seed: 42}.NewRand("room-test")
}

// newTestRoom starts a room and registers its shutdown with the test.
func newTestRoom(t *testing.T, seqToWin int) *Room {
	t.Helper()
	r, err := New(testRand(), Options{
		ID:   "room-test",
		Game: engine.Options{NumPlayers: 2, SequencesToWin: seqToWin},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// newScriptedRoom starts a room around a hand-built position, so tests can
// reach states a dealt game would only reach by luck.
func newScriptedRoom(t *testing.T, game *engine.GameState) *Room {
	t.Helper()
	r := start(game, Options{ID: "room-scripted"})
	t.Cleanup(r.Close)
	return r
}

// seatBoth joins two players and asserts the room starts playing.
func seatBoth(t *testing.T, r *Room) {
	t.Helper()
	ctx := context.Background()
	for i, p := range []PlayerID{"alice", "bob"} {
		res, err := r.Join(ctx, JoinRequest{Player: p})
		if err != nil {
			t.Fatalf("Join(%s): %v", p, err)
		}
		if res.Seat != engine.PlayerID(i) {
			t.Fatalf("Join(%s) seat = %d, want %d", p, res.Seat, i)
		}
	}
}

func TestNewRoomDealsAndWaits(t *testing.T) {
	r := newTestRoom(t, 2)
	s, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if s.Status != StatusWaiting {
		t.Errorf("status = %v, want waiting", s.Status)
	}
	// The deal happens at construction, so the game state exists (version 1)
	// before anyone joins.
	if s.Seq != 1 {
		t.Errorf("seq = %d, want 1", s.Seq)
	}
	for seat := engine.PlayerID(0); seat < 2; seat++ {
		if got := len(s.Hands[seat]); got != 7 {
			t.Errorf("seat %d hand = %d cards, want 7", seat, got)
		}
	}
	if s.DrawCount != 90 {
		t.Errorf("draw count = %d, want 90", s.DrawCount)
	}
	if len(s.Seats) != 2 || s.Seats[0].Occupied {
		t.Errorf("seats = %+v, want two free seats", s.Seats)
	}
}

func TestStatusString(t *testing.T) {
	for status, want := range map[Status]string{
		StatusWaiting:  "waiting",
		StatusPlaying:  "playing",
		StatusFinished: "finished",
		Status(9):      "status(9)",
	} {
		if got := status.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", uint8(status), got, want)
		}
	}
}

func TestNewRoomRejectsBadOptions(t *testing.T) {
	if _, err := New(testRand(), Options{ID: "x", Game: engine.Options{NumPlayers: 1}}); err == nil {
		t.Error("NumPlayers=1 should fail at room creation")
	}
}

func TestJoinFillsSeatsAndStartsGame(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()

	first, err := r.Join(ctx, JoinRequest{Player: "alice"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if first.State.Status != StatusWaiting {
		t.Errorf("status after one join = %v, want waiting", first.State.Status)
	}

	second, err := r.Join(ctx, JoinRequest{Player: "bob"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if second.Seat != 1 {
		t.Errorf("second seat = %d, want 1", second.Seat)
	}
	if second.State.Status != StatusPlaying {
		t.Errorf("status after both joined = %v, want playing", second.State.Status)
	}
	// Presence changes must not touch the game version.
	if second.State.Seq != 1 {
		t.Errorf("seq after joins = %d, want 1 (joins do not bump the game version)", second.State.Seq)
	}
}

func TestJoinIsIdempotentReconnect(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()

	first, _ := r.Join(ctx, JoinRequest{Player: "alice"})
	again, err := r.Join(ctx, JoinRequest{Player: "alice"})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if !again.Reconnect {
		t.Error("rejoin should be flagged as a reconnect")
	}
	if again.Seat != first.Seat {
		t.Errorf("rejoin seat = %d, want %d", again.Seat, first.Seat)
	}
	// A retried join must not silently consume the second seat.
	if _, err := r.Join(ctx, JoinRequest{Player: "bob"}); err != nil {
		t.Fatalf("bob should still get a seat: %v", err)
	}
}

func TestJoinRejectsFullRoomAndEmptyID(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	if _, err := r.Join(ctx, JoinRequest{Player: "carol"}); !errors.Is(err, ErrRoomFull) {
		t.Errorf("third join err = %v, want ErrRoomFull", err)
	}
	if _, err := r.Join(ctx, JoinRequest{Player: ""}); !errors.Is(err, ErrUnknownPlayer) {
		t.Errorf("empty player join err = %v, want ErrUnknownPlayer", err)
	}
}

func TestMoveBeforeGameStarts(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	if _, err := r.Join(ctx, JoinRequest{Player: "alice"}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	s, _ := r.Snapshot(ctx)
	mv, ok := chooseMove(s, 0)
	if !ok {
		t.Fatal("expected a playable card")
	}
	_, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "m1", Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	})
	if !errors.Is(err, ErrGameNotStarted) {
		t.Errorf("move before start err = %v, want ErrGameNotStarted", err)
	}
}

func TestMoveRequiresSeatAndMoveID(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)
	s, _ := r.Snapshot(ctx)
	mv, _ := chooseMove(s, 0)

	_, err := r.PlayMove(ctx, MoveRequest{
		Player: "mallory", MoveID: "m1", Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	})
	if !errors.Is(err, ErrNotInRoom) {
		t.Errorf("stranger's move err = %v, want ErrNotInRoom", err)
	}

	_, err = r.PlayMove(ctx, MoveRequest{
		Player: "alice", Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	})
	if !errors.Is(err, ErrMissingMoveID) {
		t.Errorf("move without id err = %v, want ErrMissingMoveID", err)
	}
}

func TestMoveAppliesAndBumpsSeq(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	s, _ := r.Snapshot(ctx)
	mv, ok := chooseMove(s, 0)
	if !ok {
		t.Fatal("expected a playable card")
	}
	res, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "m1", ExpectedSeq: s.Seq,
		Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	})
	if err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	if res.Duplicate {
		t.Error("first submission should not be a duplicate")
	}
	if res.Seq != 2 {
		t.Errorf("seq = %d, want 2", res.Seq)
	}
	if chip, ok := res.State.Chips[mv.Cell]; !ok || chip.Owner != 0 {
		t.Errorf("chip at %v = %+v, want owner 0", mv.Cell, chip)
	}
	if res.State.Turn != 1 {
		t.Errorf("turn = %d, want 1", res.State.Turn)
	}
	if len(res.State.Hands[0]) != 7 {
		t.Errorf("hand after play+draw = %d, want 7", len(res.State.Hands[0]))
	}
}

func TestDuplicateMoveIsIdempotent(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	s, _ := r.Snapshot(ctx)
	mv, _ := chooseMove(s, 0)
	req := MoveRequest{Player: "alice", MoveID: "m1", Type: mv.Type, Card: mv.Card, Cell: mv.Cell}

	first, err := r.PlayMove(ctx, req)
	if err != nil {
		t.Fatalf("first PlayMove: %v", err)
	}
	// The retry arrives after the turn has passed to bob and the target cell is
	// occupied — both of which would be rule violations for a *new* move. The
	// dedupe table must short-circuit before any of that is checked.
	second, err := r.PlayMove(ctx, req)
	if err != nil {
		t.Fatalf("retried PlayMove: %v", err)
	}
	if !second.Duplicate {
		t.Error("retry should be flagged Duplicate")
	}
	if second.Seq != first.Seq {
		t.Errorf("retry seq = %d, want %d (the version the move originally produced)", second.Seq, first.Seq)
	}
	if len(second.State.Chips) != 1 {
		t.Errorf("chips = %d, want 1 — the retry must not place a second chip", len(second.State.Chips))
	}
}

func TestMoveIDsAreScopedPerPlayer(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	s, _ := r.Snapshot(ctx)
	aliceMove, _ := chooseMove(s, 0)
	if _, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "1", Type: aliceMove.Type, Card: aliceMove.Card, Cell: aliceMove.Cell,
	}); err != nil {
		t.Fatalf("alice PlayMove: %v", err)
	}

	// Both clients number their moves from 1; bob's "1" is a different move.
	s, _ = r.Snapshot(ctx)
	bobMove, _ := chooseMove(s, 1)
	res, err := r.PlayMove(ctx, MoveRequest{
		Player: "bob", MoveID: "1", Type: bobMove.Type, Card: bobMove.Card, Cell: bobMove.Cell,
	})
	if err != nil {
		t.Fatalf("bob PlayMove: %v", err)
	}
	if res.Duplicate {
		t.Error("bob's move_id 1 must not collide with alice's move_id 1")
	}
}

func TestStaleExpectedSeqRejected(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	s, _ := r.Snapshot(ctx)
	mv, _ := chooseMove(s, 0)
	_, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "m1", ExpectedSeq: s.Seq + 7,
		Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	})
	if !errors.Is(err, ErrStaleSeq) {
		t.Fatalf("stale seq err = %v, want ErrStaleSeq", err)
	}
	after, _ := r.Snapshot(ctx)
	if after.Seq != s.Seq || len(after.Chips) != 0 {
		t.Error("a rejected move must leave the state untouched")
	}
	// A rejected move is not recorded, so the same move_id may be retried
	// correctly.
	if _, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "m1", ExpectedSeq: s.Seq,
		Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	}); err != nil {
		t.Fatalf("corrected retry: %v", err)
	}
}

func TestEngineErrorsPassThrough(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)
	s, _ := r.Snapshot(ctx)

	// Out of turn: bob moves while it is alice's turn.
	bobMove, _ := chooseMove(s, 1)
	_, err := r.PlayMove(ctx, MoveRequest{
		Player: "bob", MoveID: "b1", Type: bobMove.Type, Card: bobMove.Card, Cell: bobMove.Cell,
	})
	if !errors.Is(err, engine.ErrNotYourTurn) {
		t.Errorf("out-of-turn err = %v, want engine.ErrNotYourTurn", err)
	}

	// Card the player does not hold.
	_, err = r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "a1", Type: engine.MovePlace,
		Card: engine.Card{Rank: engine.Ace, Suit: engine.Spades}, Cell: engine.Cell{Row: 0, Col: 0},
	})
	if err == nil {
		t.Error("expected an engine rejection for a bogus move")
	}

	after, _ := r.Snapshot(ctx)
	if after.Seq != 1 || len(after.Chips) != 0 {
		t.Errorf("rejected moves changed state: seq=%d chips=%d", after.Seq, len(after.Chips))
	}
}

// TestDeadCardSwapKeepsTurn covers the one move type that does not end the
// turn. It is scripted because a dealt game only produces a dead card deep into
// a clogged board. The room-level property under test: a swap still bumps the
// game version (it changed state, so a client's stale ExpectedSeq must fail),
// but the same player is still to move.
func TestDeadCardSwapKeepsTurn(t *testing.T) {
	game, err := engine.NewGame(testRand(), engine.Options{NumPlayers: 2, SequencesToWin: 2})
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	// Kill two of player 0's cards by covering both board cells of each with an
	// opponent chip.
	var dead []engine.Card
	for _, c := range game.Hands[0] {
		if c.IsJack() || len(dead) == 2 {
			continue
		}
		for _, cell := range game.Board.CellsFor(c) {
			game.Chips[cell] = engine.Chip{Owner: 1}
		}
		dead = append(dead, c)
	}
	if len(dead) != 2 {
		t.Fatalf("scripted hand yielded %d dead cards, want 2", len(dead))
	}

	r := newScriptedRoom(t, game)
	ctx := context.Background()
	seatBoth(t, r)

	res, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "swap-1", ExpectedSeq: 1,
		Type: engine.MoveDeadCard, Card: dead[0],
	})
	if err != nil {
		t.Fatalf("dead-card swap: %v", err)
	}
	if res.Seq != 2 {
		t.Errorf("seq after swap = %d, want 2", res.Seq)
	}
	if res.State.Turn != 0 {
		t.Errorf("turn after swap = %d, want 0 (a swap does not end the turn)", res.State.Turn)
	}
	if len(res.State.Hands[0]) != 7 {
		t.Errorf("hand after swap = %d cards, want 7", len(res.State.Hands[0]))
	}

	// Only one swap per turn — and a client still holding the pre-swap version
	// must be told its view is stale.
	if _, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "swap-2", ExpectedSeq: 1,
		Type: engine.MoveDeadCard, Card: dead[1],
	}); !errors.Is(err, ErrStaleSeq) {
		t.Errorf("swap with stale seq err = %v, want ErrStaleSeq", err)
	}
	if _, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "swap-3", ExpectedSeq: 2,
		Type: engine.MoveDeadCard, Card: dead[1],
	}); !errors.Is(err, engine.ErrDeadCardUsed) {
		t.Errorf("second swap err = %v, want engine.ErrDeadCardUsed", err)
	}
}

func TestLeaveBeforeStartFreesSeat(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()

	if _, err := r.Join(ctx, JoinRequest{Player: "alice"}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := r.Leave(ctx, LeaveRequest{Player: "alice"}); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	res, err := r.Join(ctx, JoinRequest{Player: "carol"})
	if err != nil {
		t.Fatalf("Join after leave: %v", err)
	}
	if res.Seat != 0 {
		t.Errorf("carol seat = %d, want the vacated seat 0", res.Seat)
	}
	if res.Reconnect {
		t.Error("carol is not reconnecting")
	}
}

func TestLeaveDuringPlayHoldsSeatForReconnect(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	before, _ := r.Snapshot(ctx)
	hand := append([]engine.Card(nil), before.Hands[0]...)

	if err := r.Leave(ctx, LeaveRequest{Player: "alice"}); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	s, _ := r.Snapshot(ctx)
	if !s.Seats[0].Occupied || s.Seats[0].Present {
		t.Errorf("seat 0 = %+v, want occupied but absent", s.Seats[0])
	}
	// The seat is held, so a newcomer cannot steal the abandoned hand.
	if _, err := r.Join(ctx, JoinRequest{Player: "carol"}); !errors.Is(err, ErrRoomFull) {
		t.Errorf("join into a held seat err = %v, want ErrRoomFull", err)
	}

	back, err := r.Join(ctx, JoinRequest{Player: "alice"})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !back.Reconnect || back.Seat != 0 {
		t.Errorf("reconnect = %+v, want seat 0 reconnect", back)
	}
	if !back.State.Seats[0].Present {
		t.Error("reconnected player should be marked present")
	}
	for i, c := range back.State.Hands[0] {
		if c != hand[i] {
			t.Fatalf("hand changed across disconnect at %d: %v != %v", i, c, hand[i])
		}
	}
}

func TestLeaveByStranger(t *testing.T) {
	r := newTestRoom(t, 2)
	if err := r.Leave(context.Background(), LeaveRequest{Player: "nobody"}); !errors.Is(err, ErrNotInRoom) {
		t.Errorf("Leave err = %v, want ErrNotInRoom", err)
	}
}

func TestSnapshotIsADeepCopy(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx := context.Background()
	seatBoth(t, r)

	s, _ := r.Snapshot(ctx)
	mv, _ := chooseMove(s, 0)
	if _, err := r.PlayMove(ctx, MoveRequest{
		Player: "alice", MoveID: "m1", Type: mv.Type, Card: mv.Card, Cell: mv.Cell,
	}); err != nil {
		t.Fatalf("PlayMove: %v", err)
	}

	// Vandalize the caller's copy; the room must be unaffected.
	got, _ := r.Snapshot(ctx)
	got.Chips[engine.Cell{Row: 9, Col: 9}] = engine.Chip{Owner: 1}
	got.Hands[0] = nil
	got.Seats[0].Player = "hacked"
	if len(got.Sequences) > 0 {
		got.Sequences[0] = engine.Sequence{}
	}

	fresh, _ := r.Snapshot(ctx)
	if _, ok := fresh.Chips[engine.Cell{Row: 9, Col: 9}]; ok {
		t.Error("mutating a snapshot's chips leaked into the room")
	}
	if len(fresh.Hands[0]) != 7 {
		t.Error("mutating a snapshot's hands leaked into the room")
	}
	if fresh.Seats[0].Player != "alice" {
		t.Error("mutating a snapshot's seats leaked into the room")
	}
}

func TestClosedRoomRejectsCommands(t *testing.T) {
	r, err := New(testRand(), Options{ID: "room-closed", Game: engine.Options{NumPlayers: 2}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Close()
	r.Close() // idempotent

	select {
	case <-r.Done():
	default:
		t.Fatal("Done should be closed after Close returns")
	}
	if _, err := r.Join(context.Background(), JoinRequest{Player: "alice"}); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("join on closed room err = %v, want ErrRoomClosed", err)
	}
	if _, err := r.Snapshot(context.Background()); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("snapshot on closed room err = %v, want ErrRoomClosed", err)
	}
}

func TestContextCancellationUnblocksCaller(t *testing.T) {
	r := newTestRoom(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Snapshot err = %v, want context.Canceled", err)
	}

	// A cancelled caller must not wedge the room: the next command still works.
	ok, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := r.Snapshot(ok); err != nil {
		t.Errorf("room unusable after a cancelled caller: %v", err)
	}
}
