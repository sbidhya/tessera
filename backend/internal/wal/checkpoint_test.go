package wal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/room"
)

func TestCheckpointRemovesFileAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ev := room.Event{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: "r_check", Seq: 2, PlayerID: "alice"}
	if err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !s.Exists("r_check") {
		t.Fatal("Exists should be true after append")
	}
	if err := s.Checkpoint("r_check"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if s.Exists("r_check") {
		t.Fatal("Exists should be false after checkpoint")
	}
	if _, err := os.Stat(filepath.Join(dir, "r_check.wal")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone, stat err = %v", err)
	}
	// Idempotent second checkpoint
	if err := s.Checkpoint("r_check"); err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	// ReadAll should not see removed match
	events, err := s.ReadAll()
	if err != nil {
		// ReadAll requires no open files; we have checkpointed and closed handle, so it should succeed
		t.Fatalf("ReadAll after checkpoint: %v", err)
	}
	for _, e := range events {
		if e.RoomID == "r_check" {
			t.Fatalf("ReadAll returned checkpointed event: %+v", e)
		}
	}
}

func TestCheckpointClosesOpenHandle(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ev := room.Event{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: "r_handle", Seq: 2, PlayerID: "alice"}
	if err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Checkpoint while handle is still open
	if err := s.Checkpoint("r_handle"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Append after checkpoint should create a new file
	ev2 := room.Event{Version: room.EventVersion, Type: room.EventPlayerJoined, RoomID: "r_handle", Seq: 3, PlayerID: "bob"}
	if err := s.Append(ev2); err != nil {
		t.Fatalf("Append after checkpoint: %v", err)
	}
	if !s.Exists("r_handle") {
		t.Fatal("Exists after re-append should be true")
	}
	_ = s.Close()
}

func TestCheckpointInvalidID(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Checkpoint("../escape"); err == nil {
		t.Fatal("Checkpoint should reject unsafe id")
	}
}
