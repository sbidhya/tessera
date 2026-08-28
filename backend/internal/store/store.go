// Package store implements the cold tier for Tessera: SQLite persistence for
// finished matches and player statistics.
//
// Layering: store depends on room (and engine via room's Snapshot) and on the
// standard library plus database/sql. Room, engine, config, and wal never import
// store. Persistence is an outer layer that observes finished matches and writes
// them asynchronously.
//
// Design — write-behind with checkpoint:
//
//   - The hot path (WAL + room actor) remains authoritative. Every accepted
//     transition is fsynced to the WAL before ack, so a crash before SQLite
//     still recovers from the WAL on restart.
//   - Finished matches are enqueued to a background Flusher (see flusher.go)
//     that batches SQLite writes. After a successful transaction, the Flusher
//     calls wal.Store.Checkpoint to remove the per-match WAL file, so the match
//     will not be replayed again.
//   - SaveFinished / SaveBatch are idempotent. Persisting the same finished
//     match twice does not double-count player stats. Recovery after a crash
//     between WAL and SQLite simply re-persists the same match; the duplicate is
//     ignored.
//
// SQLite schema:
//
//	matches          — one row per finished match
//	match_players    — the players that occupied seats in that match
//	player_stats     — cumulative per-player counters (games, wins, losses)
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func mkDirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("store: create directory %s: %w", dir, err)
	}
	return nil
}

// Store is the cold SQLite tier.
type Store struct {
	db   *sql.DB
	path string
}

// MatchRecord is a finished match as stored in SQLite.
type MatchRecord struct {
	ID             string    `json:"id"`
	Seq            uint64    `json:"seq"`
	Status         string    `json:"status"`
	NumPlayers     int       `json:"num_players"`
	SequencesToWin int       `json:"sequences_to_win"`
	Winner         *int      `json:"winner"`
	Players        []string  `json:"players"`
	FinishedAt     time.Time `json:"finished_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// PlayerStats holds cumulative counters for one player.
type PlayerStats struct {
	PlayerID    string `json:"player_id"`
	GamesPlayed int    `json:"games_played"`
	Wins        int    `json:"wins"`
	Losses      int    `json:"losses"`
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS matches (
	id TEXT PRIMARY KEY,
	seq INTEGER NOT NULL,
	status TEXT NOT NULL,
	num_players INTEGER NOT NULL,
	sequences_to_win INTEGER NOT NULL,
	winner INTEGER,
	created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	finished_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS match_players (
	match_id TEXT NOT NULL REFERENCES matches(id) ON DELETE CASCADE,
	player_id TEXT NOT NULL,
	seat INTEGER NOT NULL,
	present INTEGER NOT NULL,
	PRIMARY KEY (match_id, player_id)
);

CREATE TABLE IF NOT EXISTS player_stats (
	player_id TEXT PRIMARY KEY,
	games_played INTEGER NOT NULL DEFAULT 0,
	wins INTEGER NOT NULL DEFAULT 0,
	losses INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_matches_finished ON matches(finished_at);
CREATE INDEX IF NOT EXISTS idx_match_players_player ON match_players(player_id);
`

// Open creates the directory for path if needed, opens the SQLite file, and
// initialises the schema. It enables WAL journal mode and foreign keys.
//
// The path may be ":memory:" for tests.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: path must not be empty")
	}
	// Ensure directory exists for file-backed DBs (skip for :memory:).
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			// Use 0750 to match WAL directory permissions.
			// Errors are returned to caller.
			if err := mkDirAll(dir); err != nil {
				return nil, err
			}
		}
	}

	// Busy timeout and foreign keys are applied after open via pragmas, but we
	// also request them in DSN for the mattn driver to use on each connection.
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
		// For :memory: with shared cache, the name matters; use explicit DSN.
		// The memory DB is ephemeral and used only for tests.
	}
	// The mattn driver registers "sqlite3". For ":memory:" we need a unique name
	// per Store so tests don't share state. Use path as provided if it already
	// looks like a DSN; otherwise construct.
	if path == ":memory:" {
		dsn = "file:memdb_" + fmt.Sprintf("%d", time.Now().UnixNano()) + "?mode=memory&cache=shared&_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Verify connectivity.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	// Apply schema. Each statement must be executed; the driver does not support
	// multiple statements in one Exec for some pragmas, so split.
	store := &Store{db: db, path: path}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	// Set pragmas explicitly in case DSN did not apply (e.g. in-memory).
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("store: journal_mode: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("store: foreign_keys: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		return fmt.Errorf("store: busy_timeout: %w", err)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: init schema: %w", err)
	}
	return nil
}

// Close releases the database handle. It is idempotent.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SaveFinished persists one finished match and updates player stats in a single
// transaction. It is idempotent: if the match id already exists, the call
// returns nil without double-counting stats.
func (s *Store) SaveFinished(ctx context.Context, snap room.Snapshot) error {
	if snap.Status != room.StatusFinished {
		return fmt.Errorf("store: match %s is %s, want finished", snap.RoomID, snap.Status.String())
	}
	if snap.RoomID == "" {
		return errors.New("store: snapshot has empty room id")
	}
	return s.SaveBatch(ctx, []room.Snapshot{snap})
}

// SaveBatch persists a batch of finished matches atomically per match, but all
// within one outer transaction for efficiency. Each match in the batch is
// idempotent individually.
func (s *Store) SaveBatch(ctx context.Context, snaps []room.Snapshot) error {
	if len(snaps) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, snap := range snaps {
		if snap.Status != room.StatusFinished {
			return fmt.Errorf("store: match %s is %s, want finished", snap.RoomID, snap.Status.String())
		}
		if err := saveOneTx(ctx, tx, snap); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit batch: %w", err)
	}
	return nil
}

func saveOneTx(ctx context.Context, tx *sql.Tx, snap room.Snapshot) error {
	now := time.Now().UTC()
	winnerVal := sql.NullInt64{}
	if snap.Winner != engine.NoPlayer {
		winnerVal.Int64 = int64(snap.Winner)
		winnerVal.Valid = true
	}
	// INSERT OR IGNORE makes the operation idempotent. We detect whether the
	// row was newly inserted via RowsAffected; only a new row should affect
	// player_stats, so a crash-replayed duplicate does not double-count.
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO matches (id, seq, status, num_players, sequences_to_win, winner, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snap.RoomID, snap.Seq, snap.Status.String(), snap.NumPlayers, snap.SequencesToWin, winnerVal, now,
	)
	if err != nil {
		return fmt.Errorf("store: insert match %s: %w", snap.RoomID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected %s: %w", snap.RoomID, err)
	}
	if affected == 0 {
		// Already persisted — idempotent no-op.
		return nil
	}
	// Record seat occupancy for history.
	for _, p := range snap.Players {
		present := 0
		if p.Present {
			present = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO match_players (match_id, player_id, seat, present)
			VALUES (?, ?, ?, ?)`,
			snap.RoomID, p.ID, int(p.Seat), present,
		); err != nil {
			return fmt.Errorf("store: insert match_player %s/%s: %w", snap.RoomID, p.ID, err)
		}
	}
	// Upsert per-player stats. games_played increments for every participant;
	// wins increments only for the winner, losses for the others.
	for _, p := range snap.Players {
		won := 0
		lost := 0
		if snap.Winner != engine.NoPlayer {
			if p.Seat == snap.Winner {
				won = 1
			} else {
				lost = 1
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO player_stats (player_id, games_played, wins, losses)
			VALUES (?, 1, ?, ?)
			ON CONFLICT(player_id) DO UPDATE SET
				games_played = games_played + 1,
				wins = wins + ?,
				losses = losses + ?`,
			p.ID, won, lost, won, lost,
		); err != nil {
			return fmt.Errorf("store: upsert stats %s: %w", p.ID, err)
		}
	}
	return nil
}

// ListHistory returns finished matches ordered by finished_at descending (most
// recent first). Limit <=0 means no limit. Offset is applied after ordering.
func (s *Store) ListHistory(ctx context.Context, limit, offset int) ([]MatchRecord, error) {
	query := `SELECT id, seq, status, num_players, sequences_to_win, winner, created_at, finished_at FROM matches ORDER BY finished_at DESC, id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	} else if offset > 0 {
		query += ` LIMIT -1 OFFSET ?`
		args = append(args, offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list history: %w", err)
	}
	defer rows.Close()

	var out []MatchRecord
	for rows.Next() {
		var rec MatchRecord
		var winner sql.NullInt64
		var createdAt, finishedAt time.Time
		if err := rows.Scan(&rec.ID, &rec.Seq, &rec.Status, &rec.NumPlayers, &rec.SequencesToWin, &winner, &createdAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("store: scan history: %w", err)
		}
		if winner.Valid {
			w := int(winner.Int64)
			rec.Winner = &w
		}
		rec.CreatedAt = createdAt
		rec.FinishedAt = finishedAt
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Populate player lists per match (separate query to keep history scan simple).
	for i, rec := range out {
		players, err := s.playersForMatch(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
		out[i].Players = players
	}
	return out, nil
}

func (s *Store) playersForMatch(ctx context.Context, matchID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT player_id FROM match_players WHERE match_id=? ORDER BY seat`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var players []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		players = append(players, pid)
	}
	return players, rows.Err()
}

// GetMatch returns a single finished match by id, or sql.ErrNoRows.
func (s *Store) GetMatch(ctx context.Context, id string) (MatchRecord, error) {
	var rec MatchRecord
	var winner sql.NullInt64
	var createdAt, finishedAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT id, seq, status, num_players, sequences_to_win, winner, created_at, finished_at FROM matches WHERE id=?`, id).
		Scan(&rec.ID, &rec.Seq, &rec.Status, &rec.NumPlayers, &rec.SequencesToWin, &winner, &createdAt, &finishedAt)
	if err != nil {
		return MatchRecord{}, err
	}
	if winner.Valid {
		w := int(winner.Int64)
		rec.Winner = &w
	}
	rec.CreatedAt = createdAt
	rec.FinishedAt = finishedAt
	players, err := s.playersForMatch(ctx, id)
	if err != nil {
		return MatchRecord{}, err
	}
	rec.Players = players
	return rec, nil
}

// GetPlayerStats returns cumulative stats for a player, or zero values if the
// player has no history.
func (s *Store) GetPlayerStats(ctx context.Context, playerID string) (PlayerStats, error) {
	var stats PlayerStats
	err := s.db.QueryRowContext(ctx, `SELECT player_id, games_played, wins, losses FROM player_stats WHERE player_id=?`, playerID).
		Scan(&stats.PlayerID, &stats.GamesPlayed, &stats.Wins, &stats.Losses)
	if errors.Is(err, sql.ErrNoRows) {
		return PlayerStats{PlayerID: playerID}, nil
	}
	if err != nil {
		return PlayerStats{}, fmt.Errorf("store: get stats %s: %w", playerID, err)
	}
	return stats, nil
}

// ListPlayerStats returns all player stats ordered by wins descending.
func (s *Store) ListPlayerStats(ctx context.Context) ([]PlayerStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT player_id, games_played, wins, losses FROM player_stats ORDER BY wins DESC, player_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerStats
	for rows.Next() {
		var stats PlayerStats
		if err := rows.Scan(&stats.PlayerID, &stats.GamesPlayed, &stats.Wins, &stats.Losses); err != nil {
			return nil, err
		}
		out = append(out, stats)
	}
	return out, rows.Err()
}

// HasMatch reports whether a finished match id already exists in the cold tier.
func (s *Store) HasMatch(ctx context.Context, id string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM matches WHERE id=?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
