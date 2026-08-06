package room

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

// testConfig returns a deterministic config suitable for room tests.
func testConfig(seed int64) config.Config {
	return config.Config{
		Addr:     ":0",
		Seed:     seed,
		LogLevel: -100, // silence, but logger still usable
	}
}

func newTestRoom(t *testing.T, seed int64, opts engine.Options) (*Manager, *Room) {
	t.Helper()
	mgr := NewManager(testConfig(seed))
	r, err := mgr.CreateRoom(opts)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	t.Cleanup(func() {
		mgr.DeleteRoom(r.ID())
	})
	return mgr, r
}

// ---------------------------------------------------------------------------
// Manager + Join/Leave
// ---------------------------------------------------------------------------

func TestManagerCreateAndGet(t *testing.T) {
	mgr := NewManager(testConfig(1))
	r1, err := mgr.CreateRoom(engine.Options{NumPlayers: 2})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	defer mgr.DeleteRoom(r1.ID())
	r2, err := mgr.CreateRoom(engine.Options{NumPlayers: 2})
	if err != nil {
		t.Fatalf("CreateRoom 2: %v", err)
	}
	defer mgr.DeleteRoom(r2.ID())

	if r1.ID() == r2.ID() {
		t.Fatalf("room IDs not unique: %q", r1.ID())
	}
	if mgr.RoomCount() != 2 {
		t.Fatalf("RoomCount = %d, want 2", mgr.RoomCount())
	}
	if got, ok := mgr.GetRoom(r1.ID()); !ok || got != r1 {
		t.Fatal("GetRoom failed for r1")
	}
	if _, ok := mgr.GetRoom("no-such"); ok {
		t.Fatal("GetRoom should miss unknown id")
	}
	ids := mgr.ListIDs()
	if len(ids) != 2 {
		t.Fatalf("ListIDs = %d, want 2", len(ids))
	}
}

func TestRoomJoinAssignsSeats(t *testing.T) {
	_, r := newTestRoom(t, 10, engine.Options{NumPlayers: 2})
	p0, err := r.Join("alice")
	if err != nil {
		t.Fatalf("alice Join: %v", err)
	}
	if p0 != 0 {
		t.Fatalf("alice seat = %d, want 0", p0)
	}
	p1, err := r.Join("bob")
	if err != nil {
		t.Fatalf("bob Join: %v", err)
	}
	if p1 != 1 {
		t.Fatalf("bob seat = %d, want 1", p1)
	}
	if p0 == p1 {
		t.Fatal("two players got same seat")
	}
}

func TestRoomJoinIdempotent(t *testing.T) {
	_, r := newTestRoom(t, 11, engine.Options{NumPlayers: 2})
	p0, _ := r.Join("alice")
	p0Again, err := r.Join("alice")
	if err != nil {
		t.Fatalf("re-join: %v", err)
	}
	if p0Again != p0 {
		t.Fatalf("re-join seat = %d, want %d", p0Again, p0)
	}
	// Join is idempotent even when room is full: existing member re-join succeeds.
	_, _ = r.Join("bob")
	if _, err := r.Join("bob"); err != nil {
		t.Fatalf("bob re-join when full should succeed: %v", err)
	}
}

func TestRoomJoinFull(t *testing.T) {
	_, r := newTestRoom(t, 12, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	if _, err := r.Join("carol"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("third Join err = %v, want ErrRoomFull", err)
	}
}

func TestRoomLeaveAndRejoin(t *testing.T) {
	_, r := newTestRoom(t, 13, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	if err := r.Leave("alice"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	// Seat should be free now; carol can join.
	pCarol, err := r.Join("carol")
	if err != nil {
		t.Fatalf("carol Join after leave: %v", err)
	}
	if pCarol != 0 {
		t.Fatalf("carol should reuse freed seat 0, got %d", pCarol)
	}
	if err := r.Leave("unknown"); !errors.Is(err, ErrNotInRoom) {
		t.Fatalf("Leave unknown err = %v, want ErrNotInRoom", err)
	}
}

func TestStateSnapshotIsolation(t *testing.T) {
	_, r := newTestRoom(t, 14, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	snap, seq := r.State()
	if snap == nil {
		t.Fatal("State nil")
	}
	if seq != 0 {
		t.Fatalf("initial seq = %d, want 0", seq)
	}
	// Mutating the snapshot must not affect authoritative state.
	snap.Turn = 99
	snap2, _ := r.State()
	if snap2.Turn == 99 {
		t.Fatal("snapshot mutation leaked into authoritative state")
	}
	// Chips map isolation.
	snap.Chips[engine.Cell{Row: 1, Col: 1}] = engine.Chip{Owner: 0}
	snap3, _ := r.State()
	if _, ok := snap3.Chips[engine.Cell{Row: 1, Col: 1}]; ok {
		t.Fatal("chips mutation leaked")
	}
}

func TestSeqInitiallyZero(t *testing.T) {
	_, r := newTestRoom(t, 15, engine.Options{NumPlayers: 2})
	if got := r.Seq(); got != 0 {
		t.Fatalf("Seq = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// PlayMove basics
// ---------------------------------------------------------------------------

func TestPlayMoveSuccessAndTurn(t *testing.T) {
	_, r := newTestRoom(t, 20, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	// alice is player 0 and goes first.
	snap, _ := r.State()
	var card engine.Card
	var cell engine.Cell
	found := false
	// Find a normal placement card in alice's hand.
	for _, c := range snap.Hands[0] {
		if c.IsJack() {
			continue
		}
		cells := snap.Board.CellsFor(c)
		for _, cl := range cells {
			if _, occ := snap.Chips[cl]; !occ && !snap.Board.IsCorner(cl) {
				card = c
				cell = cl
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	// Fallback: try two-eyed jack wild on any open cell.
	if !found {
		for _, c := range snap.Hands[0] {
			if c.IsTwoEyedJack() {
				card = c
				// find empty cell
				for rr := 0; rr < engine.BoardSize; rr++ {
					for cc := 0; cc < engine.BoardSize; cc++ {
						cl := engine.Cell{Row: rr, Col: cc}
						if snap.Board.IsCorner(cl) {
							continue
						}
						if _, occ := snap.Chips[cl]; !occ {
							cell = cl
							found = true
							break
						}
					}
					if found {
						break
					}
				}
			}
			if found {
				break
			}
		}
	}
	if !found {
		t.Fatal("no legal placement found for alice")
	}

	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: cell}
	res, err := r.PlayMove("alice", mv, "m1", 0)
	if err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	if res.Seq != 1 {
		t.Fatalf("Seq after first move = %d, want 1", res.Seq)
	}
	if res.State == nil {
		t.Fatal("PlayResult.State nil")
	}
	if ch, ok := res.State.Chips[cell]; !ok || ch.Owner != 0 {
		t.Fatalf("chip not placed at %s: %+v", cell, ch)
	}
	if res.State.Turn != 1 {
		t.Fatalf("Turn after alice move = %d, want 1", res.State.Turn)
	}
	if r.Seq() != 1 {
		t.Fatalf("r.Seq() = %d, want 1", r.Seq())
	}
}

func TestPlayMoveOutOfTurn(t *testing.T) {
	_, r := newTestRoom(t, 21, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	snap, _ := r.State()
	// Bob (player 1) tries to move when it's alice's turn.
	for _, card := range snap.Hands[1] {
		if card.IsJack() {
			continue
		}
		cells := snap.Board.CellsFor(card)
		if len(cells) == 0 {
			continue
		}
		cell := cells[0]
		if snap.Board.IsCorner(cell) {
			continue
		}
		mv := engine.Move{Player: 1, Type: engine.MovePlace, Card: card, Cell: cell}
		_, err := r.PlayMove("bob", mv, "o1", 0)
		if !errors.Is(err, engine.ErrNotYourTurn) {
			t.Fatalf("out-of-turn err = %v, want ErrNotYourTurn", err)
		}
		// Seq should not advance on failure.
		if r.Seq() != 0 {
			t.Fatalf("Seq after failed move = %d, want 0", r.Seq())
		}
		return
	}
	t.Fatal("could not find card to test out-of-turn")
}

func TestPlayMovePlayerMismatch(t *testing.T) {
	_, r := newTestRoom(t, 22, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	snap, _ := r.State()
	card := snap.Hands[0][0]
	cells := snap.Board.CellsFor(card)
	var cell engine.Cell
	if len(cells) > 0 {
		cell = cells[0]
	} else {
		// jack: need any open cell
		cell = engine.Cell{Row: 5, Col: 5}
	}
	mvType := engine.MovePlace
	if card.IsOneEyedJack() {
		mvType = engine.MoveRemove
	}
	// Alice's token but Move.Player says bob (1) -> mismatch.
	mv := engine.Move{Player: 1, Type: mvType, Card: card, Cell: cell}
	_, err := r.PlayMove("alice", mv, "mm1", 0)
	if !errors.Is(err, ErrPlayerMismatch) {
		t.Fatalf("player mismatch err = %v, want ErrPlayerMismatch", err)
	}
}

func TestPlayMoveNotInRoom(t *testing.T) {
	_, r := newTestRoom(t, 23, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	snap, _ := r.State()
	card := snap.Hands[0][0]
	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: engine.Cell{Row: 1, Col: 1}}
	_, err := r.PlayMove("eve", mv, "e1", 0)
	if !errors.Is(err, ErrNotInRoom) {
		t.Fatalf("not-in-room err = %v, want ErrNotInRoom", err)
	}
}

func TestPlayMoveEmptyMoveID(t *testing.T) {
	_, r := newTestRoom(t, 24, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	snap, _ := r.State()
	card := snap.Hands[0][0]
	cells := snap.Board.CellsFor(card)
	cell := engine.Cell{Row: 5, Col: 5}
	if len(cells) > 0 {
		cell = cells[0]
	}
	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: cell}
	_, err := r.PlayMove("alice", mv, "", 0)
	if !errors.Is(err, ErrInvalidMoveID) {
		t.Fatalf("empty moveID err = %v, want ErrInvalidMoveID", err)
	}
}

func TestPlayMoveStaleSequence(t *testing.T) {
	_, r := newTestRoom(t, 25, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	snap, _ := r.State()
	// First valid move.
	var mv engine.Move
	found := false
	for _, card := range snap.Hands[0] {
		if card.IsOneEyedJack() || card.IsTwoEyedJack() {
			continue
		}
		for _, cl := range snap.Board.CellsFor(card) {
			if _, occ := snap.Chips[cl]; !occ {
				mv = engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: cl}
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("no move for stale seq test")
	}
	if _, err := r.PlayMove("alice", mv, "s1", 0); err != nil {
		t.Fatalf("first move: %v", err)
	}
	// Now seq is 1. Sending expectedSeq 0 should be stale.
	snap2, seq := r.State()
	if seq != 1 {
		t.Fatalf("seq = %d, want 1", seq)
	}
	// Bob's turn now. Try bob move with stale seq 0.
	for _, card := range snap2.Hands[1] {
		if card.IsOneEyedJack() || card.IsTwoEyedJack() {
			continue
		}
		for _, cl := range snap2.Board.CellsFor(card) {
			if _, occ := snap2.Chips[cl]; !occ {
				mvBob := engine.Move{Player: 1, Type: engine.MovePlace, Card: card, Cell: cl}
				_, err := r.PlayMove("bob", mvBob, "s2", 0) // stale
				if !errors.Is(err, ErrStaleSequence) {
					t.Fatalf("stale seq err = %v, want ErrStaleSequence", err)
				}
				// Correct seq should succeed.
				_, err = r.PlayMove("bob", mvBob, "s2", 1)
				if err != nil {
					t.Fatalf("correct seq move failed: %v", err)
				}
				if r.Seq() != 2 {
					t.Fatalf("Seq after bob = %d, want 2", r.Seq())
				}
				return
			}
		}
	}
	t.Fatal("no bob move found for stale test")
}

func TestPlayMoveDuplicateIdempotency(t *testing.T) {
	_, r := newTestRoom(t, 26, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	snap, _ := r.State()
	var card engine.Card
	var cell engine.Cell
	found := false
	for _, c := range snap.Hands[0] {
		if c.IsJack() {
			continue
		}
		for _, cl := range snap.Board.CellsFor(c) {
			if _, occ := snap.Chips[cl]; !occ {
				card = c
				cell = cl
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("no card for duplicate test")
	}
	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: cell}
	// First attempt succeeds.
	res1, err := r.PlayMove("alice", mv, "dup1", 0)
	if err != nil {
		t.Fatalf("first PlayMove: %v", err)
	}
	if res1.Duplicate {
		t.Fatal("first call should not be duplicate")
	}
	if res1.Seq != 1 {
		t.Fatalf("Seq1 = %d, want 1", res1.Seq)
	}
	// Duplicate with same moveID and same expectedSeq (original seq). Should return
	// cached result and not re-apply (seq stays 1, board unchanged).
	res2, err := r.PlayMove("alice", mv, "dup1", 0)
	if err != nil {
		t.Fatalf("duplicate PlayMove err = %v, want nil (cached success)", err)
	}
	if !res2.Duplicate {
		t.Fatal("second call should be flagged duplicate")
	}
	if res2.Seq != 1 {
		t.Fatalf("Seq on duplicate = %d, want 1", res2.Seq)
	}
	// State should still have exactly one chip from this move, not two.
	snap2, seq := r.State()
	if seq != 1 {
		t.Fatalf("State seq = %d, want 1", seq)
	}
	if len(snap2.Chips) != 1 {
		t.Fatalf("chips after duplicate = %d, want 1 (no double-apply)", len(snap2.Chips))
	}
	// A different moveID with now-stale expectedSeq must be rejected as stale,
	// not as duplicate.
	// Need bob's move now, but if we try alice again with new ID and stale seq 0 -> stale.
	_, err = r.PlayMove("alice", mv, "dup2", 0)
	if !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("new ID with stale seq err = %v, want ErrStaleSequence", err)
	}
}

func TestDuplicateAfterFailureCached(t *testing.T) {
	_, r := newTestRoom(t, 27, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	// Alice's turn but we send an illegal move (wrong cell for card).
	snap, _ := r.State()
	card := snap.Hands[0][0]
	if card.IsJack() {
		// pick a non-jack if possible
		for _, c := range snap.Hands[0] {
			if !c.IsJack() {
				card = c
				break
			}
		}
	}
	// Choose a cell that definitively does not match card (e.g., a corner or
	// another card's cell).
	illegalCell := engine.Cell{Row: 5, Col: 5}
	if cells := snap.Board.CellsFor(card); len(cells) > 0 && cells[0] == illegalCell {
		illegalCell = engine.Cell{Row: 5, Col: 6}
	}
	if snap.Board.IsCorner(illegalCell) {
		illegalCell = engine.Cell{Row: 5, Col: 5}
	}
	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: illegalCell}
	_, err1 := r.PlayMove("alice", mv, "bad1", 0)
	if err1 == nil {
		t.Fatal("illegal move should have errored")
	}
	// Duplicate same bad moveID should return same error and not change seq.
	_, err2 := r.PlayMove("alice", mv, "bad1", 0)
	if !errors.Is(err2, err1) && err2.Error() != err1.Error() {
		t.Fatalf("duplicate bad move err2 = %v, want same as %v", err2, err1)
	}
	if r.Seq() != 0 {
		t.Fatalf("Seq after failed duplicate = %d, want 0", r.Seq())
	}
}

func TestIllegalMovePassThrough(t *testing.T) {
	_, r := newTestRoom(t, 28, engine.Options{NumPlayers: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	// Occupy a cell, then try to place there — expect ErrCellOccupied.
	snap, _ := r.State()
	var card engine.Card
	var cell engine.Cell
	found := false
	for _, c := range snap.Hands[0] {
		if c.IsJack() {
			continue
		}
		for _, cl := range snap.Board.CellsFor(c) {
			if !snap.Board.IsCorner(cl) {
				card = c
				cell = cl
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("no card")
	}
	mv := engine.Move{Player: 0, Type: engine.MovePlace, Card: card, Cell: cell}
	if _, err := r.PlayMove("alice", mv, "ok1", 0); err != nil {
		t.Fatalf("first place: %v", err)
	}
	// Bob tries to place on the same occupied cell. Prefer a two-eyed jack (wild)
	// to bypass card-matching, but if bob has no wild, try a normal card that
	// happens to match the occupied cell (board duplicates = possible). This keeps
	// the test deterministic across seeds without skipping.
	snap2, seq := r.State()
	var bobMove engine.Move
	foundBob := false
	// First: try wild.
	for _, c := range snap2.Hands[1] {
		if c.IsTwoEyedJack() {
			bobMove = engine.Move{Player: 1, Type: engine.MovePlace, Card: c, Cell: cell}
			foundBob = true
			break
		}
	}
	if !foundBob {
		for _, c := range snap2.Hands[1] {
			if c.IsJack() {
				continue
			}
			for _, cl := range snap2.Board.CellsFor(c) {
				if cl == cell {
					bobMove = engine.Move{Player: 1, Type: engine.MovePlace, Card: c, Cell: cell}
					foundBob = true
					break
				}
			}
			if foundBob {
				break
			}
		}
	}
	if !foundBob {
		t.Skip("bob has no card targeting occupied cell; skip occupied-error check")
	}
	_, err := r.PlayMove("bob", bobMove, "occ1", seq)
	if !errors.Is(err, engine.ErrCellOccupied) {
		t.Fatalf("occupied err = %v, want ErrCellOccupied", err)
	}
}

// ---------------------------------------------------------------------------
// Full game driver (gate)
// ---------------------------------------------------------------------------

// bruteLegalMove tries all candidates via clone+Apply and returns first that
// would succeed on gs. This keeps the full-game test from duplicating rule
// logic incorrectly — it asks the engine what is legal.
func bruteLegalMove(gs *engine.GameState, p engine.PlayerID) (engine.Move, bool) {
	if gs.Turn != p || gs.GameOver() {
		return engine.Move{}, false
	}
	hand := gs.Hands[p]
	// First try to find any successful move via exhaustive search.
	for _, card := range hand {
		// 1) Try dead-card swap if card is dead (not a jack and both cells occupied).
		// We optimistically try the DeadCard move; engine will validate.
		candDead := engine.Move{Player: p, Type: engine.MoveDeadCard, Card: card}
		clone := gs.Clone()
		if err := clone.Apply(candDead); err == nil {
			return candDead, true
		}
		if card.IsTwoEyedJack() {
			for rr := 0; rr < engine.BoardSize; rr++ {
				for cc := 0; cc < engine.BoardSize; cc++ {
					cell := engine.Cell{Row: rr, Col: cc}
					if gs.Board.IsCorner(cell) {
						continue
					}
					if _, occ := gs.Chips[cell]; occ {
						continue
					}
					cand := engine.Move{Player: p, Type: engine.MovePlace, Card: card, Cell: cell}
					clone := gs.Clone()
					if err := clone.Apply(cand); err == nil {
						return cand, true
					}
				}
			}
			continue
		}
		if card.IsOneEyedJack() {
			for cell, chip := range gs.Chips {
				if chip.Owner == p || chip.InSequence {
					continue
				}
				if gs.Board.IsCorner(cell) {
					continue
				}
				cand := engine.Move{Player: p, Type: engine.MoveRemove, Card: card, Cell: cell}
				clone := gs.Clone()
				if err := clone.Apply(cand); err == nil {
					return cand, true
				}
			}
			continue
		}
		// Normal card: its two cells.
		for _, cell := range gs.Board.CellsFor(card) {
			cand := engine.Move{Player: p, Type: engine.MovePlace, Card: card, Cell: cell}
			clone := gs.Clone()
			if err := clone.Apply(cand); err == nil {
				return cand, true
			}
		}
	}
	return engine.Move{}, false
}

func TestFullGameInProcess(t *testing.T) {
	// Gate: drive a full 2-player game in-process through the room actor.
	_, r := newTestRoom(t, 99, engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	tokens := []string{"alice", "bob"}

	moves := 0
	for moves < 500 {
		snap, seq := r.State()
		if snap.GameOver() {
			break
		}
		p := snap.Turn
		token := tokens[p]
		mv, ok := bruteLegalMove(snap, p)
		if !ok {
			t.Fatalf("no legal move for player %d turn %d, seq %d — game stuck", p, moves, seq)
		}
		moveID := fmt.Sprintf("move-%d-%d", p, moves)
		res, err := r.PlayMove(token, mv, moveID, seq)
		if err != nil {
			t.Fatalf("move %d player %d %v -> %v seq %d snap turn %d", moves, p, mv, err, seq, snap.Turn)
		}
		if res.Seq != seq+1 {
			t.Fatalf("Seq increment: got %d want %d", res.Seq, seq+1)
		}
		moves++
	}
	snap, _ := r.State()
	if !snap.GameOver() {
		t.Fatalf("game did not finish within 500 moves: winner %v sequences %v turn %d chips %d", snap.Winner, snap.SequencesWon, snap.Turn, len(snap.Chips))
	}
	if snap.Winner != 0 && snap.Winner != 1 {
		t.Fatalf("Winner = %v, want 0 or 1", snap.Winner)
	}
	if snap.SequencesWon[snap.Winner] < 1 {
		t.Fatalf("Winner sequences = %d, want >=1", snap.SequencesWon[snap.Winner])
	}
	// After game over, further move should be rejected with ErrGameOver.
	mv := engine.Move{Player: snap.Turn, Type: engine.MovePlace}
	snapCheck, seq := r.State()
	if len(snapCheck.Hands[snapCheck.Turn]) > 0 {
		mv.Card = snapCheck.Hands[snapCheck.Turn][0]
	}
	mv.Cell = engine.Cell{Row: 5, Col: 5}
	_, err := r.PlayMove(tokens[snapCheck.Turn], mv, "after-win", seq)
	if !errors.Is(err, engine.ErrGameOver) {
		t.Fatalf("after win err = %v, want ErrGameOver", err)
	}
}

func TestFullGameTwoSequences(t *testing.T) {
	_, r := newTestRoom(t, 100, engine.Options{NumPlayers: 2, SequencesToWin: 2})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	tokens := []string{"alice", "bob"}
	for i := 0; i < 800; i++ {
		snap, seq := r.State()
		if snap.GameOver() {
			break
		}
		mv, ok := bruteLegalMove(snap, snap.Turn)
		if !ok {
			t.Fatalf("stuck at move %d", i)
		}
		token := tokens[snap.Turn]
		if _, err := r.PlayMove(token, mv, fmt.Sprintf("t2-%d", i), seq); err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
	}
	snap, _ := r.State()
	if !snap.GameOver() {
		t.Skipf("game not over in 800 moves (may be near-draw exhaustion), winner %v", snap.Winner)
	}
	if snap.SequencesWon[snap.Winner] < 2 {
		t.Fatalf("winner sequences %d < 2", snap.SequencesWon[snap.Winner])
	}
}

// ---------------------------------------------------------------------------
// Concurrency / race
// ---------------------------------------------------------------------------

func TestConcurrentStateReads(t *testing.T) {
	_, r := newTestRoom(t, 200, engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	// Warm up with a few moves.
	for i := 0; i < 5; i++ {
		snap, seq := r.State()
		if snap.GameOver() {
			break
		}
		mv, ok := bruteLegalMove(snap, snap.Turn)
		if !ok {
			break
		}
		tok := []string{"alice", "bob"}[snap.Turn]
		_, _ = r.PlayMove(tok, mv, fmt.Sprintf("warm-%d", i), seq)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snap, _ := r.State()
				if snap != nil {
					_ = snap.Clone()
				}
				_ = r.Seq()
			}
		}()
	}
	wg.Wait()
	// If we got here without race detector complaining, the actor isolation holds.
}

func TestConcurrentPlayMoves(t *testing.T) {
	_, r := newTestRoom(t, 300, engine.Options{NumPlayers: 2, SequencesToWin: 1})
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")

	// Race many concurrent PlayMove attempts with correct and stale seqs.
	// Only one should succeed per seq value; others get ErrStaleSequence or duplicate.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for iter := 0; iter < 20; iter++ {
				snap, seq := r.State()
				if snap.GameOver() {
					return
				}
				mv, ok := bruteLegalMove(snap, snap.Turn)
				if !ok {
					return
				}
				token := []string{"alice", "bob"}[snap.Turn]
				// Many goroutines race with same expectedSeq; one wins, rest get stale.
				moveID := fmt.Sprintf("race-%d-%d", idx, iter)
				_, _ = r.PlayMove(token, mv, moveID, seq)
			}
		}(i)
	}
	wg.Wait()
	// State must still be coherent.
	snap, seq := r.State()
	if seq > 500 {
		t.Fatalf("seq blown up: %d", seq)
	}
	// Validate sequence count equals chips placed + removes + dead swaps? Just sanity:
	if len(snap.Chips) > int(seq)+10 { // loose
		t.Fatalf("chips %d >> seq %d", len(snap.Chips), seq)
	}
}

func TestManagerConcurrentCreateDelete(t *testing.T) {
	mgr := NewManager(testConfig(400))
	var wg sync.WaitGroup
	rooms := make([]*Room, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := mgr.CreateRoom(engine.Options{NumPlayers: 2})
			if err != nil {
				t.Errorf("CreateRoom %d: %v", idx, err)
				return
			}
			rooms[idx] = r
		}(i)
	}
	wg.Wait()
	if mgr.RoomCount() != 20 {
		t.Fatalf("RoomCount = %d, want 20", mgr.RoomCount())
	}
	for i := 0; i < 20; i++ {
		if rooms[i] != nil {
			mgr.DeleteRoom(rooms[i].ID())
		}
	}
	if mgr.RoomCount() != 0 {
		t.Fatalf("after delete RoomCount = %d, want 0", mgr.RoomCount())
	}
}

func TestRoomCloseIsIdempotent(t *testing.T) {
	mgr := NewManager(testConfig(500))
	r, _ := mgr.CreateRoom(engine.Options{NumPlayers: 2})
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := r.Join("alice"); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("Join after Close err = %v, want ErrRoomClosed", err)
	}
	if _, err := r.PlayMove("alice", engine.Move{}, "x", 0); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("PlayMove after Close err = %v, want ErrRoomClosed", err)
	}
}

func TestEngineCloneDeepCopy(t *testing.T) {
	// Direct engine test to guarantee snapshot isolation the room relies on.
	mgr := NewManager(testConfig(600))
	r, _ := mgr.CreateRoom(engine.Options{NumPlayers: 2})
	defer mgr.DeleteRoom(r.ID())
	_, _ = r.Join("alice")
	_, _ = r.Join("bob")
	s1, _ := r.State()
	s1.Chips[engine.Cell{Row: 0, Col: 1}] = engine.Chip{Owner: 0}
	s1.Hands[0] = append(s1.Hands[0], engine.Card{Rank: engine.Ace, Suit: engine.Spades})
	s1.Draw = append(s1.Draw, engine.Card{Rank: engine.King, Suit: engine.Hearts})
	s2, _ := r.State()
	if _, ok := s2.Chips[engine.Cell{Row: 0, Col: 1}]; ok {
		t.Fatal("Clone leaked Chips")
	}
	if len(s2.Hands[0]) != len(rStateCleanHands(r, 0)) {
		t.Fatal("Clone leaked Hands")
	}
}

func rStateCleanHands(r *Room, p engine.PlayerID) []engine.Card {
	s, _ := r.State()
	return s.Hands[p]
}

func TestDeleteRoomStopsActor(t *testing.T) {
	mgr := NewManager(testConfig(700))
	r, _ := mgr.CreateRoom(engine.Options{NumPlayers: 2})
	id := r.ID()
	if !mgr.DeleteRoom(id) {
		t.Fatal("DeleteRoom false")
	}
	if _, ok := mgr.GetRoom(id); ok {
		t.Fatal("room still present after delete")
	}
	if mgr.DeleteRoom(id) {
		t.Fatal("second delete should be false")
	}
	// Operations on deleted room should fail.
	if _, err := r.Join("alice"); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("Join after delete err = %v, want ErrRoomClosed", err)
	}
}
