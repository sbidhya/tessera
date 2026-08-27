// Package wal provides per-match write-ahead logging for Tessera.
//
// Every accepted state change (create, join, leave, move) is appended as a
// JSON line to <dir>/<matchID>.wal BEFORE it is applied to the in-memory
// state and before it is acked to the caller. On restart the log is replayed
// to rebuild the same GameState. The log is append-only; truncation and
// checkpointing (B5) happen after the cold tier has flushed.
//
// The WAL sits in the persistence layer and depends on the inner room layer,
// never the reverse: room defines the WAL interface, wal implements it.
package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// SyncPolicy controls durability vs speed.
type SyncPolicy int

const (
	SyncAlways SyncPolicy = iota // fsync after every append (safe)
	SyncOff                      // no fsync (fast tests)
)

// Store is an append-only per-match log directory.
type Store struct {
	dir    string
	policy SyncPolicy
	mu     sync.Mutex
}

// New creates (or opens) the WAL directory. dir must be non-empty; policy
// selects the fsync behaviour. It creates the directory if it does not exist.
func New(dir string, policy SyncPolicy) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("wal: dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir, policy: policy}, nil
}

// ParseSyncPolicy parses the config string.
func ParseSyncPolicy(s string) (SyncPolicy, error) {
	switch s {
	case "", "always":
		return SyncAlways, nil
	case "off":
		return SyncOff, nil
	default:
		return SyncAlways, fmt.Errorf("wal: unknown sync policy %q", s)
	}
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// path returns the file for a match.
func (s *Store) path(matchID string) string {
	// Basic sanitisation: room ids are r_<hex>, but be defensive.
	if strings.Contains(matchID, "/") || strings.Contains(matchID, string(os.PathSeparator)) {
		matchID = strings.ReplaceAll(matchID, "/", "_")
	}
	return filepath.Join(s.dir, matchID+".wal")
}

// Record is one WAL entry. It is serialized as a single JSON line.
type Record struct {
	Type        string   `json:"type"` // create, join, leave, move
	Timestamp   int64    `json:"ts,omitempty"`
	Options     *Options `json:"options,omitempty"`
	PlayerID    string   `json:"player_id,omitempty"`
	MoveID      string   `json:"move_id,omitempty"`
	ExpectedSeq uint64   `json:"expected_seq,omitempty"`
	MoveType    string   `json:"move_type,omitempty"` // place, remove, dead_card
	Card        *Card    `json:"card,omitempty"`
	Cell        *Cell    `json:"cell,omitempty"`
	Seq         uint64   `json:"seq,omitempty"`
}

// Options mirrors engine.Options for the log.
type Options struct {
	NumPlayers     int `json:"num_players"`
	SequencesToWin int `json:"sequences_to_win"`
}

// Card is the wire form of an engine card.
type Card struct {
	Rank string `json:"rank"`
	Suit string `json:"suit"`
}

// Cell is a board coordinate.
type Cell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

func (s *Store) append(matchID string, rec Record) error {
	rec.Timestamp = time.Now().UnixNano()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path(matchID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if s.policy == SyncAlways {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// LogCreate implements room.WAL.
func (s *Store) LogCreate(matchID string, opts engine.Options) error {
	return s.append(matchID, Record{
		Type:    "create",
		Options: &Options{NumPlayers: opts.NumPlayers, SequencesToWin: opts.SequencesToWin},
	})
}

// LogJoin implements room.WAL.
func (s *Store) LogJoin(matchID string, playerID string) error {
	return s.append(matchID, Record{Type: "join", PlayerID: playerID})
}

// LogLeave implements room.WAL.
func (s *Store) LogLeave(matchID string, playerID string) error {
	return s.append(matchID, Record{Type: "leave", PlayerID: playerID})
}

// LogMove implements room.WAL.
func (s *Store) LogMove(matchID string, req room.MoveRequest) error {
	rec := Record{
		Type:        "move",
		PlayerID:    req.PlayerID,
		MoveID:      req.MoveID,
		ExpectedSeq: req.ExpectedSeq,
		MoveType:    moveTypeToString(req.Type),
	}
	if req.Card != (engine.Card{}) || req.Type != engine.MoveDeadCard {
		// Always log the card, even for dead_card (the card being discarded).
		c := cardToWire(req.Card)
		rec.Card = &c
	}
	if req.Type != engine.MoveDeadCard {
		cell := Cell{Row: req.Cell.Row, Col: req.Cell.Col}
		rec.Cell = &cell
	}
	return s.append(matchID, rec)
}

// --- helpers for card/move conversion ---

func cardToWire(c engine.Card) Card {
	return Card{Rank: rankName(c.Rank), Suit: suitName(c.Suit)}
}

func rankName(r engine.Rank) string {
	switch r {
	case engine.Ace:
		return "A"
	case engine.Two:
		return "2"
	case engine.Three:
		return "3"
	case engine.Four:
		return "4"
	case engine.Five:
		return "5"
	case engine.Six:
		return "6"
	case engine.Seven:
		return "7"
	case engine.Eight:
		return "8"
	case engine.Nine:
		return "9"
	case engine.Ten:
		return "10"
	case engine.Jack:
		return "J"
	case engine.Queen:
		return "Q"
	case engine.King:
		return "K"
	default:
		return "?"
	}
}

func suitName(s engine.Suit) string {
	switch s {
	case engine.Spades:
		return "spades"
	case engine.Hearts:
		return "hearts"
	case engine.Diamonds:
		return "diamonds"
	case engine.Clubs:
		return "clubs"
	default:
		return "unknown"
	}
}

func moveTypeToString(t engine.MoveType) string {
	switch t {
	case engine.MovePlace:
		return "place"
	case engine.MoveRemove:
		return "remove"
	case engine.MoveDeadCard:
		return "dead_card"
	default:
		return "place"
	}
}

// ListRooms returns every match id that has a WAL file.
func (s *Store) ListRooms() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".wal") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".wal"))
	}
	return ids, nil
}

// ReadRecords returns all valid records for a match. A trailing partial line
// (crash mid-write) is treated as not committed and is ignored. The file is
// not truncated here; truncation is best-effort during replay.
func (s *Store) ReadRecords(matchID string) ([]Record, error) {
	data, err := os.ReadFile(s.path(matchID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var out []Record
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// Corrupt tail — treat as incomplete write and stop.
			break
		}
		out = append(out, rec)
	}
	return out, nil
}

// TruncateToValid truncates the file to the last valid JSON line, discarding a
// torn write at the end. It is called during replay to heal a crash.
func (s *Store) TruncateToValid(matchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(matchID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(data), "\n")
	// Re-serialize only the valid prefix.
	var valid []byte
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(trim), &rec); err != nil {
			break
		}
		valid = append(valid, []byte(trim)...)
		valid = append(valid, '\n')
	}
	// If the file was already valid, no need to rewrite.
	if len(valid) == len(data) || (len(valid) == 0 && len(strings.TrimSpace(string(data))) == 0) {
		return nil
	}
	// If the valid prefix is empty but file had a corrupt line, truncate to empty.
	if len(valid) == 0 {
		return os.Truncate(path, 0)
	}
	return os.WriteFile(path, valid, 0o644)
}
