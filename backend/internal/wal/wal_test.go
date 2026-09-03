package wal

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func testEvent(roomID string, seq uint64, player string) room.Event {
	return room.Event{
		Version:  room.EventVersion,
		Type:     room.EventPlayerJoined,
		RoomID:   roomID,
		Seq:      seq,
		PlayerID: player,
	}
}

func TestStoreRoundTripPerMatch(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []room.Event{
		testEvent("r_b", 2, "bob"),
		testEvent("r_a", 2, "alice"),
		testEvent("r_a", 3, "carol"),
	}
	for _, event := range want {
		if err := store.Append(event); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// Files are replayed by room id; order within each file is append order.
	wantOrder := []room.Event{want[1], want[2], want[0]}
	if len(got) != len(wantOrder) {
		t.Fatalf("events = %d, want %d", len(got), len(wantOrder))
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], wantOrder[i])
		}
	}
	for _, id := range []string{"r_a", "r_b"} {
		info, err := os.Stat(filepath.Join(dir, id+".wal"))
		if err != nil {
			t.Fatalf("stat %s WAL: %v", id, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", id, info.Mode().Perm())
		}
	}
}

func TestReadAllRepairsPartialTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first := testEvent("r_tail", 2, "alice")
	if err := store.Append(first); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, "r_tail.wal")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open tail: %v", err)
	}
	// Simulate a process dying halfway through the next frame header.
	if _, err := f.Write([]byte(recordMagic[:3])); err != nil {
		t.Fatalf("write partial tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close partial tail: %v", err)
	}

	recovered, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := recovered.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 1 || got[0] != first {
		t.Fatalf("events after tail repair = %+v, want [%+v]", got, first)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat repaired: %v", err)
	}
	if after.Size() != before.Size() {
		t.Errorf("repaired size = %d, want %d", after.Size(), before.Size())
	}

	second := testEvent("r_tail", 3, "bob")
	if err := recovered.Append(second); err != nil {
		t.Fatalf("Append after repair: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("Close recovered: %v", err)
	}
	final, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	t.Cleanup(func() { _ = final.Close() })
	events, err := final.ReadAll()
	if err != nil {
		t.Fatalf("final ReadAll: %v", err)
	}
	if len(events) != 2 || events[0] != first || events[1] != second {
		t.Errorf("events after continued append = %+v", events)
	}
}

func TestReadAllRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncNever)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	event := testEvent("r_bad", 2, "alice")
	if err := store.Append(event); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, "r_bad.wal")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open corrupt target: %v", err)
	}
	header := make([]byte, recordHeadLen)
	if _, err := f.ReadAt(header, 0); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := binary.BigEndian.Uint32(header[4:8])
	last := int64(recordHeadLen) + int64(length) - 1
	byteAtEnd := []byte{0}
	if _, err := f.ReadAt(byteAtEnd, last); err != nil {
		t.Fatalf("read payload byte: %v", err)
	}
	byteAtEnd[0] ^= 0xff
	if _, err := f.WriteAt(byteAtEnd, last); err != nil {
		t.Fatalf("corrupt payload byte: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close corrupt target: %v", err)
	}

	reopened, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.ReadAll(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll error = %v, want ErrCorrupt", err)
	}
}

func TestStoreValidationAndClose(t *testing.T) {
	if _, err := Open(t.TempDir(), SyncPolicy("sometimes")); err == nil {
		t.Error("Open accepted an invalid sync policy")
	}
	if _, err := ParseSyncPolicy("ALWAYS"); err != nil {
		t.Errorf("ParseSyncPolicy should be case-insensitive: %v", err)
	}
	store, err := Open(t.TempDir(), SyncNever)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Append(room.Event{RoomID: "../escape"}); err == nil {
		t.Error("Append accepted an unsafe room id")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := store.Append(testEvent("r_closed", 2, "alice")); !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
}

func TestConcurrentMatchAppends(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncNever)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const rooms = 12
	const perRoom = 40
	var wg sync.WaitGroup
	for i := range rooms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "r_concurrent_" + string(rune('a'+i))
			for seq := 1; seq <= perRoom; seq++ {
				if err := store.Append(testEvent(id, uint64(seq), "player")); err != nil {
					t.Errorf("Append %s/%d: %v", id, seq, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, SyncNever)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	events, err := reopened.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != rooms*perRoom {
		t.Errorf("events = %d, want %d", len(events), rooms*perRoom)
	}
}

func TestMoveEventJSONRoundTrip(t *testing.T) {
	event := room.Event{
		Version: room.EventVersion,
		Type:    room.EventMoveApplied,
		RoomID:  "r_move",
		Seq:     4,
		Move: room.MoveRequest{
			PlayerID:    "alice",
			MoveID:      "move-1",
			ExpectedSeq: 3,
			Type:        engine.MovePlace,
			Card:        engine.Card{Rank: engine.Ace, Suit: engine.Spades},
			Cell:        engine.Cell{Row: 4, Col: 5},
		},
	}
	dir := t.TempDir()
	store, err := Open(dir, SyncNever)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Append(event); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, SyncNever)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 1 || got[0] != event {
		t.Errorf("move event round trip = %+v, want %+v", got, event)
	}
}

func TestCheckpointRemovesOnlyTerminalWAL(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for seq := uint64(1); seq <= 4; seq++ {
		if err := store.Append(testEvent("r_done", seq, "alice")); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
	}
	if err := store.Checkpoint("r_done", 3); err == nil {
		t.Fatal("Checkpoint accepted a sequence before the final event")
	}
	if err := store.Checkpoint("r_done", 4); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := store.Checkpoint("r_done", 4); err != nil {
		t.Fatalf("idempotent Checkpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r_done.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat checkpointed WAL error = %v, want ErrNotExist", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	events, err := reopened.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("checkpointed events = %+v, want none", events)
	}
	if err := reopened.Checkpoint("r_done", 4); err != nil {
		t.Fatalf("checkpoint missing WAL after restart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r_done.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repeated checkpoint recreated WAL: %v", err)
	}
}

func TestCheckpointReleasesFileDescriptorAndStoreEntry(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, SyncAlways)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	beforeFDs, canCountFDs := openFileDescriptorCount()
	if err := store.Append(testEvent("r_release", 1, "alice")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	log := store.files["r_release"]
	if log == nil {
		t.Fatal("Append did not retain the room log")
	}
	if canCountFDs {
		if got := mustOpenFileDescriptorCount(t); got != beforeFDs+1 {
			t.Fatalf("open file descriptors after append = %d, want %d", got, beforeFDs+1)
		}
	}

	if err := store.Checkpoint("r_release", 1); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := len(store.files); got != 0 {
		t.Errorf("Store.files size after checkpoint = %d, want 0", got)
	}
	if _, err := log.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("checkpointed file Stat error = %v, want ErrClosed", err)
	}
	if canCountFDs {
		if got := mustOpenFileDescriptorCount(t); got != beforeFDs {
			t.Errorf("open file descriptors after checkpoint = %d, want %d", got, beforeFDs)
		}
	}
	path := filepath.Join(dir, "r_release.wal")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkpointed WAL Stat error = %v, want ErrNotExist", err)
	}

	if err := store.Checkpoint("r_release", 1); err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	if got := len(store.files); got != 0 {
		t.Errorf("Store.files size after second checkpoint = %d, want 0", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("second checkpoint recreated WAL: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after checkpoint: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close after checkpoint: %v", err)
	}
	if canCountFDs {
		if got := mustOpenFileDescriptorCount(t); got != beforeFDs {
			t.Errorf("open file descriptors after Close = %d, want %d", got, beforeFDs)
		}
	}
}

func openFileDescriptorCount() (int, bool) {
	for _, path := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(path)
		if err == nil {
			return len(entries), true
		}
	}
	return 0, false
}

func mustOpenFileDescriptorCount(t *testing.T) int {
	t.Helper()
	count, ok := openFileDescriptorCount()
	if !ok {
		t.Fatal("file descriptor directory disappeared during test")
	}
	return count
}
