package store

import (
	"testing"
	"time"
)

// TestOnArchivedFiresAfterCommit pins the retention signal: the post-commit
// callback runs once per archived match after the SQLite transaction commits,
// so the room manager can treat the commit (not the enqueue) as the
// archive-eligibility edge.
func TestOnArchivedFiresAfterCommit(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	fired := make(chan string, 4)
	s := testStore(t, checkpoint, 8)
	s.SetOnArchived(func(id string) { fired <- id })

	s.MatchFinished(finishedMatch("r_cb_a", "alice"))
	s.MatchFinished(finishedMatch("r_cb_b", "bob"))
	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case id := <-fired:
			got[id] = true
		case <-timeout:
			t.Fatalf("OnArchived fired for %v, want both matches", got)
		}
	}
}

// TestOnArchivedViaOptions covers the Open-time wiring path used by main.
func TestOnArchivedViaOptions(t *testing.T) {
	checkpoint := &checkpointRecorder{}
	fired := make(chan string, 1)
	s, err := Open(t.TempDir()+"/tessera.db", checkpoint, nil,
		Options{BatchSize: 8, FlushInterval: time.Hour, OnArchived: func(id string) { fired <- id }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.MatchFinished(finishedMatch("r_cb_opt", "alice"))
	if err := s.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	select {
	case id := <-fired:
		if id != "r_cb_opt" {
			t.Fatalf("OnArchived id = %q, want r_cb_opt", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnArchived did not fire after commit")
	}
}
