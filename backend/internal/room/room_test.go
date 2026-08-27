package room

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// testLogger keeps test output quiet; the room logs at info level.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testRNG returns a fixed RNG stream so every test deals the same game.
func testRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed+1)) }

// newTestRoom creates a room that is closed when the test ends.
func newTestRoom(t *testing.T, seqToWin int) *Room {
	t.Helper()
	r, err := New("r_test", testLogger(), testRNG(1), engine.Options{
		NumPlayers:     2,
		SequencesToWin: seqToWin,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// seatedRoom returns a room with both players joined, i.e. a match in progress.
func seatedRoom(t *testing.T, seqToWin int) *Room {
	t.Helper()
	r := newTestRoom(t, seqToWin)
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	return r
}

func mustJoin(t *testing.T, r *Room, playerID string) JoinResult {
	t.Helper()
	res, err := r.Join(t.Context(), playerID)
	if err != nil {
		t.Fatalf("Join(%s): %v", playerID, err)
	}
	return res
}

func mustSnapshot(t *testing.T, r *Room, viewer string) Snapshot {
	t.Helper()
	snap, err := r.Snapshot(t.Context(), viewer)
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", viewer, err)
	}
	return snap
}

func TestNewRoomRejectsBadOptions(t *testing.T) {
	if _, err := New("r_bad", testLogger(), testRNG(1), engine.Options{NumPlayers: 1}); err == nil {
		t.Fatal("NumPlayers=1 should fail at room construction")
	}
}

func TestJoinSeatsPlayersAndStartsMatch(t *testing.T) {
	r := newTestRoom(t, 2)

	first := mustJoin(t, r, "alice")
	if first.Seat != 0 {
		t.Errorf("alice seat = %d, want 0", first.Seat)
	}
	if first.Status != StatusWaiting {
		t.Errorf("status after one join = %s, want waiting", first.Status)
	}
	if first.Rejoined {
		t.Error("first join should not be flagged as a rejoin")
	}

	second := mustJoin(t, r, "bob")
	if second.Seat != 1 {
		t.Errorf("bob seat = %d, want 1", second.Seat)
	}
	if second.Status != StatusPlaying {
		t.Errorf("status after both joins = %s, want playing", second.Status)
	}
	if second.Seq <= first.Seq {
		t.Errorf("seq did not advance: %d -> %d", first.Seq, second.Seq)
	}

	// Both hands were dealt at construction and belong to the right seats.
	snap := mustSnapshot(t, r, "alice")
	if len(snap.Hand) != 7 {
		t.Errorf("alice hand = %d cards, want 7", len(snap.Hand))
	}
	if snap.HandCounts[1] != 7 {
		t.Errorf("bob hand count = %d, want 7", snap.HandCounts[1])
	}
}

func TestJoinIsIdempotent(t *testing.T) {
	r := seatedRoom(t, 2)
	before := mustSnapshot(t, r, "alice")

	// A reconnect is just another Join: same seat, no state change.
	again, err := r.Join(t.Context(), "alice")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if !again.Rejoined {
		t.Error("rejoin should be flagged as a rejoin")
	}
	if again.Seat != 0 {
		t.Errorf("rejoin seat = %d, want 0", again.Seat)
	}
	if again.Seq != before.Seq {
		t.Errorf("rejoin bumped seq: %d -> %d", before.Seq, again.Seq)
	}
}

func TestJoinErrors(t *testing.T) {
	r := seatedRoom(t, 2)

	if _, err := r.Join(t.Context(), ""); !errors.Is(err, ErrInvalidPlayerID) {
		t.Errorf("empty player id err = %v, want %v", err, ErrInvalidPlayerID)
	}
	if _, err := r.Join(t.Context(), "carol"); !errors.Is(err, ErrRoomFull) {
		t.Errorf("third player err = %v, want %v", err, ErrRoomFull)
	}
}

func TestLeaveBeforeStartFreesSeat(t *testing.T) {
	r := newTestRoom(t, 2)
	mustJoin(t, r, "alice")

	if err := r.Leave(t.Context(), "alice"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if err := r.Leave(t.Context(), "alice"); !errors.Is(err, ErrNotSeated) {
		t.Errorf("second Leave err = %v, want %v", err, ErrNotSeated)
	}

	// The freed seat is reusable, and seat 0 goes to whoever takes it next.
	res := mustJoin(t, r, "bob")
	if res.Seat != 0 {
		t.Errorf("bob seat = %d, want 0 (alice's freed seat)", res.Seat)
	}
}

func TestLeaveDuringMatchHoldsSeatForReconnect(t *testing.T) {
	r := seatedRoom(t, 2)
	before := mustSnapshot(t, r, "alice")

	if err := r.Leave(t.Context(), "alice"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	snap := mustSnapshot(t, r, "bob")
	if snap.Status != StatusPlaying {
		t.Errorf("status after mid-match leave = %s, want playing", snap.Status)
	}
	idx := slices.IndexFunc(snap.Players, func(p PlayerInfo) bool { return p.ID == "alice" })
	if idx < 0 {
		t.Fatal("alice should still hold her seat after leaving mid-match")
	}
	if snap.Players[idx].Present {
		t.Error("alice should be marked absent after leaving")
	}

	// Nobody else can take the held seat.
	if _, err := r.Join(t.Context(), "carol"); !errors.Is(err, ErrRoomFull) {
		t.Errorf("join into held seat err = %v, want %v", err, ErrRoomFull)
	}
	// ...but alice can come back to it.
	back := mustJoin(t, r, "alice")
	if !back.Rejoined || back.Seat != 0 {
		t.Errorf("alice reconnect = %+v, want rejoin into seat 0", back)
	}
	if back.Seq != before.Seq+2 { // one bump for Leave, one for becoming present
		t.Errorf("reconnect seq = %d, want %d", back.Seq, before.Seq+2)
	}
	after := mustSnapshot(t, r, "bob")
	idx = slices.IndexFunc(after.Players, func(p PlayerInfo) bool { return p.ID == "alice" })
	if idx < 0 || !after.Players[idx].Present {
		t.Error("alice should be present after reconnect")
	}
}

func TestMoveBeforeMatchStarts(t *testing.T) {
	r := newTestRoom(t, 2)
	mustJoin(t, r, "alice")
	snap := mustSnapshot(t, r, "alice")

	req := legalMove(t, snap)
	req.PlayerID = "alice"
	req.MoveID = "m1"
	if _, err := r.PlayMove(t.Context(), req); !errors.Is(err, ErrGameNotStarted) {
		t.Fatalf("move before start err = %v, want %v", err, ErrGameNotStarted)
	}
}

func TestMoveRequiresSeatAndMoveID(t *testing.T) {
	r := seatedRoom(t, 2)
	snap := mustSnapshot(t, r, "alice")

	noID := legalMove(t, snap)
	noID.PlayerID = "alice"
	if _, err := r.PlayMove(t.Context(), noID); !errors.Is(err, ErrMissingMoveID) {
		t.Errorf("missing move id err = %v, want %v", err, ErrMissingMoveID)
	}

	stranger := legalMove(t, snap)
	stranger.PlayerID = "carol"
	stranger.MoveID = "m1"
	if _, err := r.PlayMove(t.Context(), stranger); !errors.Is(err, ErrNotSeated) {
		t.Errorf("unseated player err = %v, want %v", err, ErrNotSeated)
	}
}

// TestMoveOutOfTurnRejected checks the room defers turn enforcement to the
// engine and that a rejection changes nothing.
func TestMoveOutOfTurnRejected(t *testing.T) {
	r := seatedRoom(t, 2)
	bobView := mustSnapshot(t, r, "bob")

	req := legalMove(t, bobView)
	req.PlayerID = "bob"
	req.MoveID = "m1"
	if _, err := r.PlayMove(t.Context(), req); !errors.Is(err, engine.ErrNotYourTurn) {
		t.Fatalf("out-of-turn err = %v, want %v", err, engine.ErrNotYourTurn)
	}
	after := mustSnapshot(t, r, "bob")
	if after.Seq != bobView.Seq {
		t.Errorf("rejected move bumped seq: %d -> %d", bobView.Seq, after.Seq)
	}
	if len(after.Chips) != 0 {
		t.Errorf("rejected move placed a chip: %v", after.Chips)
	}
}

// TestPlayerCannotSpoofAnotherSeat pins the authoritative-server property: the
// acting seat comes from the room's own seating table, never from the request.
// Bob replaying alice's exact move payload is still out of turn.
func TestPlayerCannotSpoofAnotherSeat(t *testing.T) {
	r := seatedRoom(t, 2)
	aliceView := mustSnapshot(t, r, "alice")

	spoof := legalMove(t, aliceView) // a card from ALICE's hand
	spoof.PlayerID = "bob"
	spoof.MoveID = "m1"
	if _, err := r.PlayMove(t.Context(), spoof); !errors.Is(err, engine.ErrNotYourTurn) {
		t.Fatalf("spoofed move err = %v, want %v", err, engine.ErrNotYourTurn)
	}
}

func TestDuplicateMoveIDIsIdempotent(t *testing.T) {
	r := seatedRoom(t, 2)
	snap := mustSnapshot(t, r, "alice")

	req := legalMove(t, snap)
	req.PlayerID = "alice"
	req.MoveID = "move-42"

	first, err := r.PlayMove(t.Context(), req)
	if err != nil {
		t.Fatalf("first PlayMove: %v", err)
	}
	if first.Duplicate {
		t.Error("first submission flagged as duplicate")
	}

	// The client never saw the ack and retries with the same move id.
	retry, err := r.PlayMove(t.Context(), req)
	if err != nil {
		t.Fatalf("retry PlayMove: %v", err)
	}
	if !retry.Duplicate {
		t.Error("retry not flagged as duplicate")
	}
	if retry.Seq != first.Seq || retry.Turn != first.Turn {
		t.Errorf("retry result = %+v, want the original %+v", retry, first)
	}

	after := mustSnapshot(t, r, "alice")
	if len(after.Chips) != 1 {
		t.Errorf("chips = %d, want 1 (the retry must not place a second chip)", len(after.Chips))
	}
	if len(after.Hand) != 7 {
		t.Errorf("hand = %d cards, want 7 (the retry must not spend a second card)", len(after.Hand))
	}
}

// TestDuplicateAckSurvivesLaterState covers the ordering that makes idempotency
// real: a retry arriving after the opponent has moved (so the seq is stale) must
// still replay the original ack rather than fail.
func TestDuplicateAckSurvivesLaterState(t *testing.T) {
	r := seatedRoom(t, 2)

	aliceMove := legalMove(t, mustSnapshot(t, r, "alice"))
	aliceMove.PlayerID = "alice"
	aliceMove.MoveID = "a1"
	aliceMove.ExpectedSeq = mustSnapshot(t, r, "alice").Seq
	first, err := r.PlayMove(t.Context(), aliceMove)
	if err != nil {
		t.Fatalf("alice move: %v", err)
	}

	bobMove := legalMove(t, mustSnapshot(t, r, "bob"))
	bobMove.PlayerID = "bob"
	bobMove.MoveID = "b1"
	if _, err := r.PlayMove(t.Context(), bobMove); err != nil {
		t.Fatalf("bob move: %v", err)
	}

	// Alice's retry now carries an ExpectedSeq two versions behind.
	retry, err := r.PlayMove(t.Context(), aliceMove)
	if err != nil {
		t.Fatalf("late retry err = %v, want the original ack replayed", err)
	}
	if !retry.Duplicate || retry.Seq != first.Seq {
		t.Errorf("late retry = %+v, want duplicate of %+v", retry, first)
	}
}

// TestRejectedMoveDoesNotConsumeMoveID: a move that never applied had no effect,
// so its id stays free and a corrected retry is evaluated normally.
func TestRejectedMoveDoesNotConsumeMoveID(t *testing.T) {
	r := seatedRoom(t, 2)
	snap := mustSnapshot(t, r, "alice")

	bogus := MoveRequest{
		PlayerID: "alice",
		MoveID:   "m1",
		Type:     engine.MovePlace,
		Card:     engine.Card{Rank: engine.Queen, Suit: engine.Clubs},
		Cell:     engine.Cell{Row: 4, Col: 4},
	}
	// Pick a card the player certainly does not hold.
	for _, c := range snap.Hand {
		if c == bogus.Card {
			bogus.Card = engine.Card{Rank: engine.King, Suit: engine.Spades}
		}
	}
	if _, err := r.PlayMove(t.Context(), bogus); err == nil {
		t.Fatal("expected the bogus move to be rejected")
	}

	fixed := legalMove(t, snap)
	fixed.PlayerID = "alice"
	fixed.MoveID = "m1" // same id, now a legal move
	res, err := r.PlayMove(t.Context(), fixed)
	if err != nil {
		t.Fatalf("corrected retry: %v", err)
	}
	if res.Duplicate {
		t.Error("corrected retry should not be treated as a duplicate")
	}
}

func TestExpectedSeqRejectsStaleMove(t *testing.T) {
	r := seatedRoom(t, 2)
	snap := mustSnapshot(t, r, "alice")

	req := legalMove(t, snap)
	req.PlayerID = "alice"
	req.MoveID = "m1"
	req.ExpectedSeq = snap.Seq + 7 // a version the room has never been at

	if _, err := r.PlayMove(t.Context(), req); !errors.Is(err, ErrStaleSeq) {
		t.Fatalf("stale seq err = %v, want %v", err, ErrStaleSeq)
	}

	// Zero means "no opinion" and is always accepted.
	req.ExpectedSeq = 0
	if _, err := r.PlayMove(t.Context(), req); err != nil {
		t.Fatalf("ExpectedSeq=0 should skip the check, got %v", err)
	}
}

func TestSnapshotHidesOpponentHand(t *testing.T) {
	r := seatedRoom(t, 2)

	alice := mustSnapshot(t, r, "alice")
	if len(alice.Hand) != 7 || alice.Viewer != 0 {
		t.Fatalf("alice view = seat %d with %d cards, want seat 0 with 7", alice.Viewer, len(alice.Hand))
	}
	bob := mustSnapshot(t, r, "bob")
	if slices.Equal(alice.Hand, bob.Hand) {
		t.Error("both players were shown the same hand")
	}

	// A spectator sees the public state and nobody's cards.
	spec := mustSnapshot(t, r, "")
	if spec.Viewer != engine.NoPlayer {
		t.Errorf("spectator viewer = %d, want NoPlayer", spec.Viewer)
	}
	if spec.Hand != nil {
		t.Errorf("spectator was shown a hand: %v", spec.Hand)
	}
	if spec.HandCounts[0] != 7 || spec.HandCounts[1] != 7 {
		t.Errorf("spectator hand counts = %v, want both 7", spec.HandCounts)
	}
}

// TestSnapshotIsADeepCopy guards the invariant the whole actor model rests on:
// nothing a caller holds may alias the room's live state.
func TestSnapshotIsADeepCopy(t *testing.T) {
	r := seatedRoom(t, 2)
	snap := mustSnapshot(t, r, "alice")

	snap.Chips[engine.Cell{Row: 0, Col: 1}] = engine.Chip{Owner: 1}
	snap.SequencesWon[0] = 99
	if len(snap.Hand) > 0 {
		snap.Hand[0] = engine.Card{Rank: engine.Ace, Suit: engine.Spades}
	}

	fresh := mustSnapshot(t, r, "alice")
	if len(fresh.Chips) != 0 {
		t.Errorf("mutating a snapshot leaked into the room: chips = %v", fresh.Chips)
	}
	if fresh.SequencesWon[0] != 0 {
		t.Errorf("mutating a snapshot leaked into the room: SequencesWon = %v", fresh.SequencesWon)
	}
	if slices.Equal(fresh.Hand, snap.Hand) {
		t.Error("mutating a snapshot's hand leaked into the room")
	}
}

func TestClosedRoomRejectsCommands(t *testing.T) {
	r := newTestRoom(t, 2)
	r.Close()
	r.Close() // idempotent, must not panic or block

	if _, err := r.Join(t.Context(), "alice"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("Join after close = %v, want %v", err, ErrRoomClosed)
	}
	if _, err := r.Snapshot(t.Context(), "alice"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("Snapshot after close = %v, want %v", err, ErrRoomClosed)
	}
	if err := r.Leave(t.Context(), "alice"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("Leave after close = %v, want %v", err, ErrRoomClosed)
	}
	if _, err := r.PlayMove(t.Context(), MoveRequest{PlayerID: "alice", MoveID: "m1"}); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("PlayMove after close = %v, want %v", err, ErrRoomClosed)
	}
}

func TestCancelledContextReturnsPromptly(t *testing.T) {
	r := seatedRoom(t, 2)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := r.Snapshot(ctx, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot with cancelled ctx = %v, want context.Canceled", err)
	}
	// The room survives an abandoned caller.
	if _, err := r.Snapshot(t.Context(), "alice"); err != nil {
		t.Fatalf("room unusable after a cancelled call: %v", err)
	}
}

func TestStatusString(t *testing.T) {
	cases := map[Status]string{
		StatusWaiting:  "waiting",
		StatusPlaying:  "playing",
		StatusFinished: "finished",
		Status(9):      "status(9)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", uint8(s), got, want)
		}
	}
}
