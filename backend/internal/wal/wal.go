// Package wal implements Tessera's append-only, per-match write-ahead log.
//
// The room actor decides what an accepted event is and enforces
// append-before-apply ordering. This package only supplies durable framing,
// checksums, fsync policy, tail repair, and replay from disk.
package wal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sbidhya/tessera/backend/internal/room"
)

const (
	recordMagic   = "TSW1"
	recordHeadLen = 12 // 4 magic + 4 payload length + 4 checksum
	maxRecordSize = 1 << 20
)

var (
	ErrClosed  = errors.New("wal: store is closed")
	ErrCorrupt = errors.New("wal: corrupt record")
)

// SyncPolicy controls when an accepted command is forced from the operating
// system's page cache to durable storage.
type SyncPolicy string

const (
	// SyncAlways calls fsync before Append returns. This is the safe default:
	// once a client receives an ack, its event survived a process or OS crash.
	SyncAlways SyncPolicy = "always"
	// SyncNever relies on the OS flush policy. It is useful for local experiments
	// where throughput matters more than losing the newest events on hard crash.
	SyncNever SyncPolicy = "never"
)

func ParseSyncPolicy(value string) (SyncPolicy, error) {
	policy := SyncPolicy(strings.ToLower(value))
	switch policy {
	case SyncAlways, SyncNever:
		return policy, nil
	default:
		return "", fmt.Errorf("wal: invalid sync policy %q (want always or never)", value)
	}
}

// Store keeps one append-only file per match. The store mutex only protects the
// file directory; each match file has its own lock, so one room's fsync does not
// serialize unrelated matches.
type Store struct {
	dir    string
	policy SyncPolicy

	mu     sync.Mutex
	files  map[string]*logFile
	closed bool
}

type logFile struct {
	mu     sync.Mutex
	file   *os.File
	failed error
	closed bool
}

// Open creates dir if needed and returns a ready WAL store.
func Open(dir string, policy SyncPolicy) (*Store, error) {
	if dir == "" {
		return nil, errors.New("wal: directory must not be empty")
	}
	if policy != SyncAlways && policy != SyncNever {
		return nil, fmt.Errorf("wal: invalid sync policy %q", policy)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("wal: create directory %s: %w", dir, err)
	}
	return &Store{dir: dir, policy: policy, files: make(map[string]*logFile)}, nil
}

// Append writes one checksummed frame and, under SyncAlways, fsyncs it before
// returning. Any write/sync failure poisons that match's log: continuing could
// append valid bytes after a torn record and make later recovery ambiguous.
func (s *Store) Append(event room.Event) error {
	if !validRoomID(event.RoomID) {
		return fmt.Errorf("wal: unsafe room id %q", event.RoomID)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("wal: encode event: %w", err)
	}
	if len(payload) > maxRecordSize {
		return fmt.Errorf("wal: event is %d bytes, maximum is %d", len(payload), maxRecordSize)
	}

	frame := make([]byte, recordHeadLen+len(payload))
	copy(frame[:4], recordMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(payload))
	copy(frame[recordHeadLen:], payload)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	log, err := s.fileLocked(event.RoomID)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	if log.closed {
		return ErrClosed
	}
	if log.failed != nil {
		return fmt.Errorf("wal: room %s unavailable after prior failure: %w", event.RoomID, log.failed)
	}
	if err := writeFull(log.file, frame); err != nil {
		log.failed = fmt.Errorf("wal: append room %s: %w", event.RoomID, err)
		return log.failed
	}
	if s.policy == SyncAlways {
		if err := log.file.Sync(); err != nil {
			log.failed = fmt.Errorf("wal: sync room %s: %w", event.RoomID, err)
			return log.failed
		}
	}
	return nil
}

func (s *Store) fileLocked(roomID string) (*logFile, error) {
	if log := s.files[roomID]; log != nil {
		return log, nil
	}
	path := filepath.Join(s.dir, roomID+".wal")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wal: open room %s: %w", roomID, err)
	}
	if s.policy == SyncAlways {
		// fsyncing the file protects its contents; fsyncing the directory protects
		// the new filename itself. Without both, a create ack could survive while
		// the per-match WAL disappears after a machine-level crash.
		dir, err := os.Open(s.dir)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("wal: open directory for sync: %w", err)
		}
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("wal: sync directory: %w", syncErr)
		}
		if closeErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("wal: close directory after sync: %w", closeErr)
		}
	}
	log := &logFile{file: f}
	s.files[roomID] = log
	return log, nil
}

// ReadAll returns every complete event, ordered by room filename and then file
// position. A partial final frame is the expected shape of a killed process; it
// is truncated back to the last checksum-verified boundary before replay.
func (s *Store) ReadAll() ([]room.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if len(s.files) != 0 {
		return nil, errors.New("wal: replay must finish before appending events")
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("wal: read directory %s: %w", s.dir, err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	var events []room.Event
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wal") {
			continue
		}
		roomID := strings.TrimSuffix(entry.Name(), ".wal")
		if !validRoomID(roomID) {
			return nil, fmt.Errorf("wal: unsafe log filename %q", entry.Name())
		}
		fromFile, err := s.readFile(filepath.Join(s.dir, entry.Name()), roomID)
		if err != nil {
			return nil, err
		}
		events = append(events, fromFile...)
	}
	return events, nil
}

func (s *Store) readFile(path, roomID string) ([]room.Event, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("wal: open %s for replay: %w", path, err)
	}
	defer f.Close()

	var events []room.Event
	var offset int64
	for {
		start := offset
		header := make([]byte, recordHeadLen)
		n, err := io.ReadFull(f, header)
		offset += int64(n)
		if errors.Is(err, io.EOF) && n == 0 {
			return events, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return events, s.repairTail(f, start)
		}
		if err != nil {
			return nil, fmt.Errorf("wal: read %s at offset %d: %w", path, start, err)
		}
		if string(header[:4]) != recordMagic {
			return nil, fmt.Errorf("%w: %s offset %d has bad magic", ErrCorrupt, path, start)
		}
		length := binary.BigEndian.Uint32(header[4:8])
		if length > maxRecordSize {
			return nil, fmt.Errorf("%w: %s offset %d declares %d bytes", ErrCorrupt, path, start, length)
		}
		payload := make([]byte, int(length))
		n, err = io.ReadFull(f, payload)
		offset += int64(n)
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return events, s.repairTail(f, start)
		}
		if err != nil {
			return nil, fmt.Errorf("wal: read %s payload at offset %d: %w", path, start, err)
		}
		if got, want := crc32.ChecksumIEEE(payload), binary.BigEndian.Uint32(header[8:12]); got != want {
			return nil, fmt.Errorf("%w: %s offset %d checksum %08x, want %08x",
				ErrCorrupt, path, start, got, want)
		}

		var event room.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("%w: decode %s offset %d: %v", ErrCorrupt, path, start, err)
		}
		if event.RoomID != roomID {
			return nil, fmt.Errorf("%w: file %s contains event for room %q", ErrCorrupt, path, event.RoomID)
		}
		events = append(events, event)
	}
}

func (s *Store) repairTail(f *os.File, validBytes int64) error {
	if err := f.Truncate(validBytes); err != nil {
		return fmt.Errorf("wal: truncate partial tail to %d bytes: %w", validBytes, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("wal: sync repaired tail: %w", err)
	}
	return nil
}

// Checkpoint truncates the WAL for roomID after its match has been
// successfully flushed to the cold tier (SQLite). The per-match file is
// removed so a later restart does not replay an already-archived match.
//
// Checkpoint is idempotent: removing a non-existent file returns nil. It
// closes any open handle for that room first, so callers do not race with an
// ongoing Append.
func (s *Store) Checkpoint(roomID string) error {
	if !validRoomID(roomID) {
		return fmt.Errorf("wal: unsafe room id %q", roomID)
	}
	path := filepath.Join(s.dir, roomID+".wal")

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if log := s.files[roomID]; log != nil {
		log.mu.Lock()
		_ = log.file.Close()
		log.closed = true
		log.mu.Unlock()
		delete(s.files, roomID)
	}
	s.mu.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wal: checkpoint %s: %w", roomID, err)
	}
	if s.policy == SyncAlways {
		if dir, err := os.Open(s.dir); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}

// Exists reports whether a WAL file exists for roomID.
func (s *Store) Exists(roomID string) bool {
	if !validRoomID(roomID) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.dir, roomID+".wal"))
	return err == nil
}

// Close releases all open files. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	files := s.files
	s.files = nil
	s.mu.Unlock()

	var errs []error
	for roomID, log := range files {
		log.mu.Lock()
		log.closed = true
		if err := log.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("wal: close room %s: %w", roomID, err))
		}
		log.mu.Unlock()
	}
	return errors.Join(errs...)
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func validRoomID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
