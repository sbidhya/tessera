package room

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
)

type memoryJournal struct {
	mu     sync.Mutex
	events []Event
	fail   error
}

type captureArchive struct {
	matches chan FinishedMatch
}

func (a *captureArchive) MatchFinished(match FinishedMatch, _ func()) {
	a.matches <- match
}

func (j *memoryJournal) Append(event Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail != nil {
		return j.fail
	}
	j.events = append(j.events, event)
	return nil
}

func (j *memoryJournal) ReadAll() ([]Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Event(nil), j.events...), nil
}

func (j *memoryJournal) setFailure(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fail = err
}

func (j *memoryJournal) duplicateLast() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, j.events[len(j.events)-1])
}

func durableTestManager(t *testing.T, seed int64, journal EventJournal) *Manager {
	t.Helper()
	cfg := config.Config{Seed: seed}
	m, err := NewDurableManager(testLogger(), cfg.NewRand, journal)
	if err != nil {
		t.Fatalf("NewDurableManager: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func TestDurableRoomAppendsAcceptedTransitions(t *testing.T) {
	journal := &memoryJournal{}
	m := durableTestManager(t, 11, journal)
	r, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")

	req := legalMove(t, mustSnapshot(t, r, "alice"))
	req.PlayerID, req.MoveID = "alice", "m1"
	if _, err := r.PlayMove(t.Context(), req); err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	if err := r.Leave(t.Context(), "alice"); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	events, _ := journal.ReadAll()
	wantTypes := []EventType{
		EventRoomCreated, EventPlayerJoined, EventPlayerJoined, EventMoveApplied, EventPlayerLeft,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, event := range events {
		if event.Type != wantTypes[i] {
			t.Errorf("event %d type = %s, want %s", i, event.Type, wantTypes[i])
		}
		if event.Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
		if event.RoomID != r.ID() || event.Version != EventVersion {
			t.Errorf("event %d identity/version = %+v", i, event)
		}
	}
	if events[3].Move != req {
		t.Errorf("logged move = %+v, want %+v", events[3].Move, req)
	}
}

func TestJournalFailureLeavesStateUnchanged(t *testing.T) {
	journal := &memoryJournal{}
	m := durableTestManager(t, 12, journal)
	r, err := m.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	before := mustSnapshot(t, r, "alice")
	req := legalMove(t, before)
	req.PlayerID, req.MoveID = "alice", "m1"

	diskFull := errors.New("disk full")
	journal.setFailure(diskFull)
	if _, err := r.PlayMove(t.Context(), req); !errors.Is(err, ErrDurability) || !errors.Is(err, diskFull) {
		t.Fatalf("PlayMove error = %v, want ErrDurability wrapping disk error", err)
	}
	after := mustSnapshot(t, r, "alice")
	if !reflect.DeepEqual(after, before) {
		t.Errorf("state changed after failed WAL append\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestCreateFailureDoesNotRegisterRoom(t *testing.T) {
	journal := &memoryJournal{fail: errors.New("read-only filesystem")}
	m := durableTestManager(t, 13, journal)
	if _, err := m.Create(twoPlayer); !errors.Is(err, ErrDurability) {
		t.Fatalf("Create error = %v, want ErrDurability", err)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("failed Create registered %d rooms", got)
	}
}

func TestDurableManagerReplaysStateAndMoveIDs(t *testing.T) {
	journal := &memoryJournal{}
	first := durableTestManager(t, 21, journal)
	r, err := first.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	req := legalMove(t, mustSnapshot(t, r, "alice"))
	req.PlayerID, req.MoveID = "alice", "move-before-crash"
	accepted, err := r.PlayMove(t.Context(), req)
	if err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	before := mustSnapshot(t, r, "alice")
	first.Shutdown()

	// Recovery uses the per-room seed in the create event, not the current
	// process seed. Changing the latter must not change an existing match.
	recovered := durableTestManager(t, 9999, journal)
	rooms := recovered.List()
	if len(rooms) != 1 {
		t.Fatalf("recovered rooms = %d, want 1", len(rooms))
	}
	after := mustSnapshot(t, rooms[0], "alice")
	if after.Seq != before.Seq || after.Turn != before.Turn || after.Winner != before.Winner ||
		after.DrawRemaining != before.DrawRemaining || !reflect.DeepEqual(after.Board, before.Board) ||
		!reflect.DeepEqual(after.Hand, before.Hand) || !reflect.DeepEqual(after.HandCounts, before.HandCounts) ||
		!reflect.DeepEqual(after.Chips, before.Chips) || !reflect.DeepEqual(after.Sequences, before.Sequences) ||
		!reflect.DeepEqual(after.SequencesWon, before.SequencesWon) {
		t.Errorf("recovered game differs\nbefore: %+v\nafter:  %+v", before, after)
	}
	for _, player := range after.Players {
		if player.Present {
			t.Errorf("recovered player %s is present without a live connection", player.ID)
		}
	}

	// The original move's idempotency entry is rebuilt too. Its old ack is
	// returned even though the request omits the already-applied card and cell.
	retry, err := rooms[0].PlayMove(t.Context(), MoveRequest{PlayerID: "alice", MoveID: req.MoveID})
	if err != nil {
		t.Fatalf("duplicate after recovery: %v", err)
	}
	if !retry.Duplicate || retry.Seq != accepted.Seq || retry.Turn != accepted.Turn {
		t.Errorf("duplicate after recovery = %+v, want original %+v", retry, accepted)
	}
}

func TestReplaySkipsExactDuplicateRecord(t *testing.T) {
	journal := &memoryJournal{}
	first := durableTestManager(t, 31, journal)
	r, err := first.Create(twoPlayer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	req := legalMove(t, mustSnapshot(t, r, "alice"))
	req.PlayerID, req.MoveID = "alice", "once"
	if _, err := r.PlayMove(t.Context(), req); err != nil {
		t.Fatalf("PlayMove: %v", err)
	}
	first.Shutdown()
	journal.duplicateLast()

	recovered := durableTestManager(t, 31, journal)
	snap := mustSnapshot(t, recovered.List()[0], "alice")
	if snap.Seq != 4 || len(snap.Chips) != 1 {
		t.Errorf("duplicate replay produced seq=%d chips=%d, want 4/1", snap.Seq, len(snap.Chips))
	}
}

func TestReplayRejectsSequenceGap(t *testing.T) {
	journal := &memoryJournal{events: []Event{
		createdEvent("r_gap", twoPlayer, [2]uint64{1, 2}),
		{Version: EventVersion, Type: EventPlayerJoined, RoomID: "r_gap", Seq: 3, PlayerID: "alice"},
	}}
	cfg := config.Config{Seed: 1}
	if _, err := NewDurableManager(testLogger(), cfg.NewRand, journal); err == nil {
		t.Fatal("recovery accepted an event sequence gap")
	}
}

func TestRejectedCommandIsNotLogged(t *testing.T) {
	journal := &memoryJournal{}
	m := durableTestManager(t, 41, journal)
	r, err := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	before, _ := journal.ReadAll()
	if _, err := r.Join(t.Context(), ""); !errors.Is(err, ErrInvalidPlayerID) {
		t.Fatalf("bad Join error = %v", err)
	}
	after, _ := journal.ReadAll()
	if len(after) != len(before) {
		t.Errorf("rejected command appended an event: %d -> %d", len(before), len(after))
	}
}

func TestFinishedMatchIsArchivedOnceAndBecomesImmutable(t *testing.T) {
	journal := &memoryJournal{}
	archive := &captureArchive{matches: make(chan FinishedMatch, 2)}
	cfg := config.Config{Seed: 51}
	m, err := NewPersistentManager(testLogger(), cfg.NewRand, journal, archive)
	if err != nil {
		t.Fatalf("NewPersistentManager: %v", err)
	}
	t.Cleanup(m.Shutdown)
	r, err := m.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	finishTestRoom(t, r)

	archived := <-archive.matches
	if archived.RoomID != r.ID() || archived.History[len(archived.History)-1].Seq != archived.FinishedSeq {
		t.Fatalf("archive = %+v", archived)
	}
	if len(archive.matches) != 0 {
		t.Fatal("match was archived more than once")
	}
	eventsBefore, _ := journal.ReadAll()
	seqBefore := mustSnapshot(t, r, "alice").Seq
	if err := r.Leave(t.Context(), "alice"); err != nil {
		t.Fatalf("post-finish Leave: %v", err)
	}
	if _, err := r.Join(t.Context(), "alice"); err != nil {
		t.Fatalf("post-finish Join: %v", err)
	}
	eventsAfter, _ := journal.ReadAll()
	if len(eventsAfter) != len(eventsBefore) || mustSnapshot(t, r, "alice").Seq != seqBefore {
		t.Error("finished match accepted a post-terminal presence transition")
	}
}

func TestRecoveryAcceptsB4PostFinishPresenceEvents(t *testing.T) {
	journal := &memoryJournal{}
	first := durableTestManager(t, 52, journal)
	r, err := first.Create(engine.Options{NumPlayers: 2, SequencesToWin: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustJoin(t, r, "alice")
	mustJoin(t, r, "bob")
	finishTestRoom(t, r)
	terminal := mustSnapshot(t, r, "").Seq
	first.Shutdown()

	// B4 allowed disconnect/reconnect events after a match ended. Preserve the
	// ability to upgrade and archive those existing logs even though B5 no longer
	// writes post-terminal presence changes.
	journal.mu.Lock()
	journal.events = append(journal.events,
		Event{Version: EventVersion, Type: EventPlayerLeft, RoomID: r.ID(), Seq: terminal + 1, PlayerID: "alice"},
		Event{Version: EventVersion, Type: EventPlayerJoined, RoomID: r.ID(), Seq: terminal + 2, PlayerID: "alice"},
	)
	journal.mu.Unlock()

	archive := &captureArchive{matches: make(chan FinishedMatch, 1)}
	cfg := config.Config{Seed: 999}
	recovered, err := NewPersistentManager(testLogger(), cfg.NewRand, journal, archive)
	if err != nil {
		t.Fatalf("recover B4 WAL: %v", err)
	}
	t.Cleanup(recovered.Shutdown)
	archived := <-archive.matches
	if archived.FinishedSeq != terminal+2 || len(archived.History) != int(terminal+2) {
		t.Errorf("archive seq/history = %d/%d, want %d/%d",
			archived.FinishedSeq, len(archived.History), terminal+2, terminal+2)
	}
}

func finishTestRoom(t *testing.T, r *Room) {
	t.Helper()
	players := []string{"alice", "bob"}
	for move := 0; move < 5000; move++ {
		snap := mustSnapshot(t, r, "")
		if snap.Status == StatusFinished {
			return
		}
		view := mustSnapshot(t, r, players[snap.Turn])
		req, ok := chooseMove(view)
		if !ok {
			t.Fatalf("no move after %d plays", move)
		}
		req.PlayerID = players[view.Viewer]
		req.MoveID = fmt.Sprintf("archive-%d", move)
		if _, err := r.PlayMove(t.Context(), req); err != nil {
			t.Fatalf("move %d: %v", move, err)
		}
	}
	t.Fatal("match did not finish")
}
