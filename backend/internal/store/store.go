// Package store implements Tessera's SQLite cold tier.
//
// Finished matches arrive from the room layer through an in-memory queue. A
// single worker batches them into SQLite transactions, updates aggregate player
// statistics exactly once, and only then checkpoints each match's WAL. The WAL
// therefore remains the source of truth during the write-behind window.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
	_ "modernc.org/sqlite"
)

const (
	defaultBatchSize     = 16
	defaultFlushInterval = time.Second
)

var ErrClosed = errors.New("store: closed")

// Checkpointer is the narrow WAL operation needed by the cold tier. It is kept
// as an interface so store tests can verify commit-before-checkpoint ordering.
type Checkpointer interface {
	Checkpoint(roomID string, throughSeq uint64) error
}

// Options controls write-behind batching. Zero values select conservative
// defaults suitable for the local learning server.
type Options struct {
	BatchSize     int
	FlushInterval time.Duration
}

// Match is a persisted finished-match summary. A draw has an empty WinnerID
// and WinnerSeat == engine.NoPlayer, matching the NULL winner columns.
type Match struct {
	ID             string
	FinishedSeq    uint64
	NumPlayers     int
	SequencesToWin int
	WinnerID       string
	WinnerSeat     engine.PlayerID
	MoveCount      int
	ArchivedAt     time.Time
}

// IsDraw reports whether the archived match ended in a draw.
func (m Match) IsDraw() bool { return m.WinnerSeat == engine.NoPlayer }

// PlayerStats is the aggregate result row for one player.
type PlayerStats struct {
	PlayerID           string
	MatchesPlayed      int
	Wins               int
	Losses             int
	SequencesCompleted int
	UpdatedAt          time.Time
}

// Store owns the SQLite connection and one write-behind goroutine. SQLite is
// deliberately limited to one connection: it serializes this small workload,
// keeps connection-local PRAGMAs reliable, and also makes :memory: tests refer
// to one database rather than one database per pooled connection.
type Store struct {
	db         *sql.DB
	checkpoint Checkpointer
	logger     *slog.Logger
	batchSize  int
	flushEvery time.Duration
	commands   chan any
	done       chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

type archiveCmd struct {
	match    room.FinishedMatch
	archived func()
}
type pendingArchive struct {
	match     room.FinishedMatch
	callbacks []func()
}
type flushCmd struct{ reply chan error }
type closeCmd struct{ reply chan error }

// Open creates or migrates a SQLite database and starts its write-behind
// worker. checkpointer is normally the process WAL store and is required: a
// successful SQLite commit without the matching WAL checkpoint would otherwise
// be retried forever after every restart.
func Open(path string, checkpointer Checkpointer, logger *slog.Logger, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path must not be empty")
	}
	if checkpointer == nil {
		return nil, errors.New("store: WAL checkpointer is required")
	}
	if opts.BatchSize < 0 {
		return nil, errors.New("store: batch size must be positive")
	}
	if opts.FlushInterval < 0 {
		return nil, errors.New("store: flush interval must be positive")
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	if logger == nil {
		logger = slog.Default()
	}

	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("store: create database file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("store: close new database file: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{
		db:         db,
		checkpoint: checkpointer,
		logger:     logger,
		batchSize:  opts.BatchSize,
		flushEvery: opts.FlushInterval,
		commands:   make(chan any, max(64, opts.BatchSize*4)),
		done:       make(chan struct{}),
	}
	go s.loop()
	return s, nil
}

func migrate(db *sql.DB) error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`CREATE TABLE IF NOT EXISTS matches (
			id TEXT PRIMARY KEY,
			finished_seq INTEGER NOT NULL,
			num_players INTEGER NOT NULL,
			sequences_to_win INTEGER NOT NULL,
			winner_player_id TEXT NOT NULL,
			winner_seat INTEGER NOT NULL,
			move_count INTEGER NOT NULL,
			archive_hash BLOB NOT NULL,
			archived_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS match_players (
			match_id TEXT NOT NULL REFERENCES matches(id) ON DELETE CASCADE,
			player_id TEXT NOT NULL,
			seat INTEGER NOT NULL,
			won INTEGER NOT NULL CHECK (won IN (0, 1)),
			sequences_completed INTEGER NOT NULL,
			PRIMARY KEY (match_id, player_id),
			UNIQUE (match_id, seat)
		)`,
		`CREATE TABLE IF NOT EXISTS match_events (
			match_id TEXT NOT NULL REFERENCES matches(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL,
			type TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (match_id, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS player_stats (
			player_id TEXT PRIMARY KEY,
			matches_played INTEGER NOT NULL,
			wins INTEGER NOT NULL,
			losses INTEGER NOT NULL,
			sequences_completed INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("store: migrate SQLite: %w", err)
		}
	}
	return migrateWinnersNullable(db)
}

// migrateWinnersNullable relaxes matches.winner_player_id and
// matches.winner_seat to NULL so drawn matches (no winner) can be archived.
// Databases created before draws existed carry NOT NULL columns, and SQLite
// cannot drop a NOT NULL constraint in place, so the table is rebuilt once
// with data preserved. The CREATE TABLE above intentionally keeps its
// historical NOT NULL shape: fresh and existing databases converge through
// this one migration path, and re-running it is a no-op once the columns are
// nullable.
func migrateWinnersNullable(db *sql.DB) error {
	notNull, err := winnersNotNull(db)
	if err != nil {
		return err
	}
	if !notNull["winner_player_id"] && !notNull["winner_seat"] {
		return nil
	}
	// Dropping the parent table with foreign keys enforced would implicitly
	// delete the child rows, so enforcement is lifted for the rebuild and
	// restored afterwards. MaxOpenConns(1) keeps every statement on the same
	// connection, which is what makes the PRAGMA toggle reliable here.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("store: relax winner columns: %w", err)
	}
	defer func() {
		_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
	}()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: relax winner columns: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE TABLE matches_new (
			id TEXT PRIMARY KEY,
			finished_seq INTEGER NOT NULL,
			num_players INTEGER NOT NULL,
			sequences_to_win INTEGER NOT NULL,
			winner_player_id TEXT,
			winner_seat INTEGER,
			move_count INTEGER NOT NULL,
			archive_hash BLOB NOT NULL,
			archived_at TEXT NOT NULL
		)`,
		`INSERT INTO matches_new
			(id, finished_seq, num_players, sequences_to_win, winner_player_id, winner_seat, move_count, archive_hash, archived_at)
			SELECT id, finished_seq, num_players, sequences_to_win, winner_player_id, winner_seat, move_count, archive_hash, archived_at FROM matches`,
		`DROP TABLE matches`,
		`ALTER TABLE matches_new RENAME TO matches`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("store: relax winner columns: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: relax winner columns: %w", err)
	}
	return nil
}

// winnersNotNull reports the NOT NULL flags of the matches columns. The rows
// cursor is closed before returning: the pool holds a single connection, so no
// further statement may run while it is open.
func winnersNotNull(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(matches)`)
	if err != nil {
		return nil, fmt.Errorf("store: inspect matches schema: %w", err)
	}
	defer rows.Close()
	notNull := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var nn int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("store: inspect matches schema: %w", err)
		}
		notNull[name] = nn == 1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: inspect matches schema: %w", err)
	}
	return notNull, nil
}

// MatchFinished implements room.FinishedMatchSink. It only copies the value
// into the worker queue; disk I/O remains off the room actor's hot path.
func (s *Store) MatchFinished(match room.FinishedMatch, archived func()) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.commands <- archiveCmd{match: cloneMatch(match), archived: archived}:
	case <-s.done:
	}
}

// Flush forces every match queued before this call through SQLite and WAL
// checkpointing. It is primarily a shutdown/test barrier; normal operation is
// driven by batch size and the periodic timer.
func (s *Store) Flush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case s.commands <- flushCmd{reply: reply}:
	case <-s.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drains the queue once, closes SQLite, and is idempotent. If flushing
// fails, the error is returned but the uncheckpointed WAL remains available to
// retry on the next process start.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		reply := make(chan error, 1)
		s.commands <- closeCmd{reply: reply}
		flushErr := <-reply
		dbErr := s.db.Close()
		s.closeErr = errors.Join(flushErr, dbErr)
		close(s.done)
	})
	return s.closeErr
}

func (s *Store) loop() {
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()
	pending := make(map[string]pendingArchive)

	for {
		select {
		case raw := <-s.commands:
			switch cmd := raw.(type) {
			case archiveCmd:
				item := pending[cmd.match.RoomID]
				item.match = cmd.match
				if cmd.archived != nil {
					item.callbacks = append(item.callbacks, cmd.archived)
				}
				pending[cmd.match.RoomID] = item
				if len(pending) >= s.batchSize {
					if err := s.flushOne(pending); err != nil {
						s.logger.Error("archive finished matches", "err", err, "pending", len(pending))
					}
				}
			case flushCmd:
				cmd.reply <- s.flushAll(pending)
			case closeCmd:
				cmd.reply <- s.flushAll(pending)
				return
			}
		case <-ticker.C:
			if err := s.flushAll(pending); err != nil {
				s.logger.Error("archive finished matches", "err", err, "pending", len(pending))
			}
		}
	}
}

func (s *Store) flushAll(pending map[string]pendingArchive) error {
	for len(pending) > 0 {
		if err := s.flushOne(pending); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) flushOne(pending map[string]pendingArchive) error {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) > s.batchSize {
		ids = ids[:s.batchSize]
	}
	batch := make([]room.FinishedMatch, 0, len(ids))
	for _, id := range ids {
		batch = append(batch, pending[id].match)
	}
	if err := s.persistBatch(batch, func() {
		// A committed SQLite row is enough to make the live room evictable. Clear
		// callbacks before invoking them so a later WAL-checkpoint retry cannot
		// acknowledge the same archival twice.
		for _, id := range ids {
			item := pending[id]
			callbacks := item.callbacks
			item.callbacks = nil
			pending[id] = item
			for _, archived := range callbacks {
				archived()
			}
		}
	}); err != nil {
		return err
	}
	for _, id := range ids {
		delete(pending, id)
	}
	return nil
}

func (s *Store) persistBatch(batch []room.FinishedMatch, committed func()) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("store: begin archive transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, match := range batch {
		if err := archiveMatch(tx, match, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit archive transaction: %w", err)
	}
	if committed != nil {
		committed()
	}

	// The transaction is durable before any WAL bytes are discarded. If a
	// checkpoint fails, keep the item pending; retrying the SQLite transaction is
	// a no-op because matches.id is the idempotency key.
	for _, match := range batch {
		if err := s.checkpoint.Checkpoint(match.RoomID, match.FinishedSeq); err != nil {
			return fmt.Errorf("store: checkpoint WAL for %s: %w", match.RoomID, err)
		}
	}
	return nil
}

func archiveMatch(tx *sql.Tx, match room.FinishedMatch, archivedAt string) error {
	winnerID, moveCount, err := validateMatch(match)
	if err != nil {
		return err
	}
	if match.FinishedSeq > math.MaxInt64 {
		return fmt.Errorf("store: match %s sequence exceeds SQLite integer range", match.RoomID)
	}
	hash, err := archiveHash(match)
	if err != nil {
		return err
	}

	// A draw persists NULL winner columns; a win persists the winner.
	var winnerIDArg, winnerSeatArg any
	if match.Winner != engine.NoPlayer {
		winnerIDArg = winnerID
		winnerSeatArg = int(match.Winner)
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO matches
		(id, finished_seq, num_players, sequences_to_win, winner_player_id, winner_seat, move_count, archive_hash, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		match.RoomID, int64(match.FinishedSeq), match.NumPlayers, match.SequencesToWin,
		winnerIDArg, winnerSeatArg, moveCount, hash, archivedAt)
	if err != nil {
		return fmt.Errorf("store: insert match %s: %w", match.RoomID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect match insert %s: %w", match.RoomID, err)
	}
	if inserted == 0 {
		return verifyExisting(tx, match, winnerID, moveCount, hash)
	}

	for _, player := range match.Players {
		won := 0
		wins, losses := 0, 0
		switch {
		case player.Won:
			won, wins = 1, 1
		case match.Winner != engine.NoPlayer:
			// A decided loss. Draws record neither a win nor a loss:
			// both participants only bank the match played.
			losses = 1
		}
		if _, err := tx.Exec(`INSERT INTO match_players
			(match_id, player_id, seat, won, sequences_completed) VALUES (?, ?, ?, ?, ?)`,
			match.RoomID, player.ID, int(player.Seat), won, player.Sequences); err != nil {
			return fmt.Errorf("store: insert player %s for match %s: %w", player.ID, match.RoomID, err)
		}
		if _, err := tx.Exec(`INSERT INTO player_stats
			(player_id, matches_played, wins, losses, sequences_completed, updated_at)
			VALUES (?, 1, ?, ?, ?, ?)
			ON CONFLICT(player_id) DO UPDATE SET
				matches_played = player_stats.matches_played + 1,
				wins = player_stats.wins + excluded.wins,
				losses = player_stats.losses + excluded.losses,
				sequences_completed = player_stats.sequences_completed + excluded.sequences_completed,
				updated_at = excluded.updated_at`,
			player.ID, wins, losses, player.Sequences, archivedAt); err != nil {
			return fmt.Errorf("store: update stats for %s: %w", player.ID, err)
		}
	}

	for _, event := range match.History {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("store: encode event %s/%d: %w", match.RoomID, event.Seq, err)
		}
		if _, err := tx.Exec(`INSERT INTO match_events (match_id, seq, type, payload) VALUES (?, ?, ?, ?)`,
			match.RoomID, int64(event.Seq), string(event.Type), payload); err != nil {
			return fmt.Errorf("store: insert event %s/%d: %w", match.RoomID, event.Seq, err)
		}
	}
	return nil
}

func validateMatch(match room.FinishedMatch) (winnerID string, moveCount int, err error) {
	if match.RoomID == "" || match.FinishedSeq == 0 {
		return "", 0, errors.New("store: finished match has incomplete identity")
	}
	if match.NumPlayers < 2 || len(match.Players) != match.NumPlayers {
		return "", 0, fmt.Errorf("store: match %s has %d players, want %d", match.RoomID, len(match.Players), match.NumPlayers)
	}
	if match.SequencesToWin < 1 {
		return "", 0, fmt.Errorf("store: match %s has invalid sequences_to_win", match.RoomID)
	}
	if len(match.History) == 0 || match.History[0].Type != room.EventRoomCreated ||
		match.History[len(match.History)-1].Seq != match.FinishedSeq {
		return "", 0, fmt.Errorf("store: match %s has incomplete history", match.RoomID)
	}

	seenPlayers := make(map[string]struct{}, len(match.Players))
	seenSeats := make(map[engine.PlayerID]struct{}, len(match.Players))
	winners := 0
	for _, player := range match.Players {
		if player.ID == "" || player.Seat < 0 || int(player.Seat) >= match.NumPlayers {
			return "", 0, fmt.Errorf("store: match %s has invalid player result", match.RoomID)
		}
		if _, ok := seenPlayers[player.ID]; ok {
			return "", 0, fmt.Errorf("store: match %s repeats player %s", match.RoomID, player.ID)
		}
		if _, ok := seenSeats[player.Seat]; ok {
			return "", 0, fmt.Errorf("store: match %s repeats seat %d", match.RoomID, player.Seat)
		}
		seenPlayers[player.ID] = struct{}{}
		seenSeats[player.Seat] = struct{}{}
		if player.Won {
			winners++
			winnerID = player.ID
			if player.Seat != match.Winner {
				return "", 0, fmt.Errorf("store: match %s winner seat disagrees with player result", match.RoomID)
			}
		}
	}
	switch winners {
	case 0:
		// A draw: no Won player and no winner seat. Anything else is a
		// corrupt projection, not a draw.
		if match.Winner != engine.NoPlayer {
			return "", 0, fmt.Errorf("store: match %s has no winning player but winner seat %d", match.RoomID, match.Winner)
		}
	case 1:
		// A win: validated per player above.
	default:
		return "", 0, fmt.Errorf("store: match %s has %d winners", match.RoomID, winners)
	}

	var previous uint64
	for _, event := range match.History {
		if event.RoomID != match.RoomID || event.Seq != previous+1 {
			return "", 0, fmt.Errorf("store: match %s history is not contiguous at sequence %d", match.RoomID, event.Seq)
		}
		previous = event.Seq
		if event.Type == room.EventMoveApplied {
			moveCount++
		}
	}
	return winnerID, moveCount, nil
}

func archiveHash(match room.FinishedMatch) ([]byte, error) {
	payload, err := json.Marshal(match)
	if err != nil {
		return nil, fmt.Errorf("store: hash match %s: %w", match.RoomID, err)
	}
	sum := sha256.Sum256(payload)
	return sum[:], nil
}

func verifyExisting(tx *sql.Tx, match room.FinishedMatch, winnerID string, moveCount int, hash []byte) error {
	var seq int64
	var players, sequences, moves int
	var storedWinner sql.NullString
	var storedSeat sql.NullInt64
	var storedHash []byte
	err := tx.QueryRow(`SELECT finished_seq, num_players, sequences_to_win,
		winner_player_id, winner_seat, move_count, archive_hash FROM matches WHERE id = ?`, match.RoomID).
		Scan(&seq, &players, &sequences, &storedWinner, &storedSeat, &moves, &storedHash)
	if err != nil {
		return fmt.Errorf("store: verify existing match %s: %w", match.RoomID, err)
	}
	var wantWinner sql.NullString
	var wantSeat sql.NullInt64
	if match.Winner != engine.NoPlayer {
		wantWinner = sql.NullString{String: winnerID, Valid: true}
		wantSeat = sql.NullInt64{Int64: int64(match.Winner), Valid: true}
	}
	if seq != int64(match.FinishedSeq) || players != match.NumPlayers || sequences != match.SequencesToWin ||
		storedWinner != wantWinner || storedSeat != wantSeat || moves != moveCount || !bytes.Equal(storedHash, hash) {
		return fmt.Errorf("store: archived match %s conflicts with WAL result", match.RoomID)
	}
	return nil
}

// Match returns one persisted summary. A draw comes back with an empty
// WinnerID and WinnerSeat == engine.NoPlayer.
func (s *Store) Match(ctx context.Context, id string) (Match, error) {
	var result Match
	var seq int64
	var winnerID sql.NullString
	var winnerSeat sql.NullInt64
	var archived string
	err := s.db.QueryRowContext(ctx, `SELECT id, finished_seq, num_players, sequences_to_win,
		winner_player_id, winner_seat, move_count, archived_at FROM matches WHERE id = ?`, id).
		Scan(&result.ID, &seq, &result.NumPlayers, &result.SequencesToWin,
			&winnerID, &winnerSeat, &result.MoveCount, &archived)
	if err != nil {
		return Match{}, err
	}
	result.FinishedSeq = uint64(seq)
	result.WinnerSeat = engine.NoPlayer
	if winnerID.Valid {
		result.WinnerID = winnerID.String
	}
	if winnerSeat.Valid {
		result.WinnerSeat = engine.PlayerID(winnerSeat.Int64)
	}
	result.ArchivedAt, err = time.Parse(time.RFC3339Nano, archived)
	if err != nil {
		return Match{}, fmt.Errorf("store: parse archived time for %s: %w", id, err)
	}
	return result, nil
}

// HasMatch reports whether id has reached the cold tier. It is the narrow
// lookup used by transport to distinguish an evicted archived match from an
// id that never existed.
func (s *Store) HasMatch(ctx context.Context, id string) (bool, error) {
	_, err := s.Match(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// History returns the accepted event stream stored for one finished match.
func (s *Store) History(ctx context.Context, matchID string) ([]room.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM match_events WHERE match_id = ? ORDER BY seq`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []room.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event room.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("store: decode history for %s: %w", matchID, err)
		}
		history = append(history, event)
	}
	return history, rows.Err()
}

// Stats returns aggregate results for one player.
func (s *Store) Stats(ctx context.Context, playerID string) (PlayerStats, error) {
	var stats PlayerStats
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT player_id, matches_played, wins, losses,
		sequences_completed, updated_at FROM player_stats WHERE player_id = ?`, playerID).
		Scan(&stats.PlayerID, &stats.MatchesPlayed, &stats.Wins, &stats.Losses,
			&stats.SequencesCompleted, &updated)
	if err != nil {
		return PlayerStats{}, err
	}
	stats.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return PlayerStats{}, fmt.Errorf("store: parse stats time for %s: %w", playerID, err)
	}
	return stats, nil
}

func cloneMatch(match room.FinishedMatch) room.FinishedMatch {
	match.Players = slices.Clone(match.Players)
	match.History = slices.Clone(match.History)
	return match
}
