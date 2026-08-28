# B5 — Cold tier: SQLite + write-behind

Tessera now has two persistence tiers: the **hot** WAL (B4) and the **cold**
SQLite store (this block). Hot is latency-critical and synchronous; cold is
throughput-oriented and asynchronous.

```
client -> room actor -> WAL fsync -> ack/broadcast
                          |
                          v (async batch)
                       SQLite -> WAL checkpoint (delete)
```

## Why two tiers?

The WAL is an append-only log: fast, crash-safe, but poor for queries
("list last 10 matches for alice", "how many wins does bob have?"). Scanning
hundreds of per-match files for every history request would be slow and would
mix hot-path I/O with cold queries. SQLite gives indexed queries, transactional
stats, and a stable file that survives WAL truncation, while keeping the
deployment footprint to a single file (no external service).

## Layer boundary

`internal/room` still imports only engine + stdlib. Both persistence
packages depend inward:

* `internal/wal` imports `room` for the durable `Event` schema and implements
  `room.EventJournal`.
* `internal/store` imports `room` (for `Snapshot`) and `wal` (for
  `Checkpoint`). The room and transport layers never import `store`.

The outer `Flusher` lives in `store` because it coordinates the two tiers,
but its trigger is a plain `func(string)` callback set by `cmd/tessera`
(`transport.Server.SetFlushHook`), so transport does not import `store`.

```
engine <- room <- transport <- {wal, store} <- infra (cmd/tessera)
```

## SQLite schema

```sql
matches (id PK, seq, status, num_players, sequences_to_win, winner, finished_at)
match_players (match_id, player_id, seat, present)  PK(match_id, player_id)
player_stats (player_id PK, games_played, wins, losses)
```

* `matches` holds one row per finished match, ordered by `finished_at`.
* `match_players` records who occupied which seat (needed for history).
* `player_stats` is a cumulative counter, updated in the same transaction as
  `matches` so the two never diverge.

 pragmas: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`. The DB
 file is created with `0750` parent directory, `0600` via SQLite default.

## Write-behind batching

`store.Flusher` is the write-behind engine:

* `Enqueue(roomID)` — called from the transport hub when a move finishes the
  match (`StatusFinished`). It is non-blocking and coalesces duplicates.
* Background loop — every `100ms` or when `10` ids are pending, `Flush` is
  called.
* `Flush` — snapshots pending ids plus any additional finished rooms discovered
  by scanning `manager.List()` (so a crash-recovered finished match that was
  never enqueued is still flushed), sorts them, and calls `Store.SaveBatch`
  in one SQLite transaction. On success, each WAL file is checkpointed
  (removed); on failure, ids are re-queued for the next tick.
* Startup recovery flush — `cmd/tessera` runs `flusher.Flush` once immediately
  after replaying the WAL, before serving traffic, so a crash between WAL
  fsync and SQLite lands in history without waiting for the next move.

Batching matters because SQLite's transaction overhead dominates for single
rows. Grouping 10 finished matches into one `BEGIN/COMMIT` wins ~10x
throughput in local benchmarks, while the 100 ms interval keeps history
freshness below a human-noticeable threshold.

## Idempotency (duplicate replay is safe)

`Store.SaveBatch` is idempotent per match:

```sql
INSERT OR IGNORE INTO matches ...
-- check RowsAffected; if 0, the match was already cold, so skip stats
```

* A duplicate WAL replay after restart re-inserts the same `id`; `OR IGNORE`
  makes it a no-op and stats are not double-counted.
* A retried `Flush` after a transient SQLite error re-queues the same ids;
  the next successful transaction sees the same property.
* `player_stats` is updated with `ON CONFLICT DO UPDATE` only when a new
  `matches` row was inserted, so `games_played/wins/losses` count each match
  exactly once.

## Checkpoint / truncate

`wal.Store.Checkpoint(roomID)` closes any open handle for that room and
`os.Remove`s `<wal_dir>/<roomID>.wal`. It is idempotent (removing a
non-existent file returns nil) and fsyncs the directory when `SyncAlways`.
A checkpoint is only called after the SQLite transaction has committed, so a
crash before checkpoint leaves the WAL intact for replay; a crash after
checkpoint has already persisted the match, so losing the WAL is safe.

`wal.Store.Exists(roomID)` is provided for tests and for the pre-flush
guard.

## Crash between WAL and SQLite

This is the key B5 gate:

1. First incarnation: create and finish a match, WAL fsyncs each event, but
   do not flush to SQLite (simulate crash).
2. Kill the process without closing the store.
3. Second incarnation: reopen WAL and store on the same files, call
   `NewDurableManager` (replays WAL into memory, recreates the finished
   match), then `Flusher.Flush`. The finished match appears in SQLite and its
   WAL disappears. A third flush is a no-op.

See `internal/store/flusher_test.go:TestCrashBetweenWALAndSQLiteStillRecovers`.

## Configuration

* `TESSERA_DB_PATH` — SQLite file, default `data/tessera.db`.
* WAL settings unchanged (`TESSERA_WAL_DIR`, `TESSERA_WAL_SYNC`).

## HTTP surface (infra, not transport)

History and stats are served from the infra layer (`cmd/tessera/server.go`),
not from `transport`, to keep the layer graph acyclic:

* `GET /v1/history?limit=50&offset=0` → `{matches: [...MatchRecord]}` ordered
  by `finished_at DESC`.
* `GET /v1/players/{playerID}/stats` → `{stats: {player_id, games_played,
  wins, losses}}`

Both read only from SQLite; live matches still come from
`GET /v1/matches` (transport).

## Test gate

From `backend/`:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Covered:

* `store_test.go` — single/batch idempotency, non-finished rejection,
  pagination, zero-stats for unknown player.
* `flusher_test.go` — sync flush + checkpoint, batch of 3, crash-recovery,
  async enqueue + background ticker.
* `wal/checkpoint_test.go` — checkpoint removes file, is idempotent, closes
  open handle.
* `cmd/tessera/history_test.go` — HTTP history and stats after a real
  finished match through the full stack.

Layering is verified with `go list -deps`:

* `engine` imports no room/transport/wal/store.
* `room` imports only engine + stdlib.
* `transport` imports only engine + room.
* `store` imports engine + room + wal (persistence tier).
