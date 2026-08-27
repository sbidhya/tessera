# B4 — Durability: WAL + replay

`internal/wal` is the persistence leaf that makes every accepted state change
survive a process crash. It sits outside `room` and `transport` — `room`
defines the `WAL` interface, `wal` implements it, and `cmd/tessera` wires
them at startup. The engine remains pure and the room's actor model stays
the hot path.

## What the WAL logs

One append-only file per match: `<WALDir>/<matchID>.wal`, one JSON line per
accepted command.

```
{"type":"create","options":{"num_players":2,"sequences_to_win":1}}
{"type":"join","player_id":"alice"}
{"type":"join","player_id":"bob"}
{"type":"move","player_id":"alice","move_id":"m0","move_type":"place","card":{"rank":"A","suit":"spades"},"cell":{"row":4,"col":6}}
{"type":"leave","player_id":"alice"}
```

Only accepted commands are logged. The duplicate check, `StatusWaiting`,
`ExpectedSeq` and engine validation all run first; a rejected move never
creates a log entry and therefore never consumes its `move_id`. Duplicate
retries (`move_id` already in `applied`) return the original ack without
re-logging — that is what makes replay idempotent.

## Write-ahead: log before apply

The room is authoritative. For the three mutating paths the order is:

```
validate → log (fsync per policy) → mutate in-memory state → ack
```

* `join` — validate seat/presence, `LogJoin`, then bump `seq` and flip
  `present`.
* `leave` — validate seated, `LogLeave`, then mark absent or free the seat.
* `playMove` — duplicate check, `StatusWaiting`/`ExpectedSeq` checks, then
  clone the `GameState` and try `Apply` on the clone. If it succeeds, `LogMove`
  before adopting the clone as the new `gs`. If logging fails the real state
  is untouched and the caller gets the error.

`Manager.Create` also follows write-ahead: the `GameState` is validated by
`NewGame`, then `LogCreate` before the room is inserted into the directory.
Until the log is fsynced the room is not visible.

The clone-then-log pattern is the only place B4 adds engine code: `GameState.Clone`
deep-copies every map/slice so validation does not mutate the live state.

## Fsync policy

`Config.WALSync` (env `TESSERA_WAL_FSYNC`) selects the durability/speed
trade-off:

* `always` (default) — `fsync` every append. Safe for production.
* `off` — no `fsync`. Fast for tests where a `TempDir` is discarded.

Both policies still append atomically per line; a torn tail (crash
mid-write) is healed on the next replay by `TruncateToValid`.

## Replay

On startup `cmd/tessera` does:

```go
store, _ := wal.New(cfg.WALDir, policy)
store.Replay(manager) // rebuilds every room, then manager.SetWAL(store)
```

`Replay` lists `*.wal`, truncates any torn tail, reads each file's JSON
lines in order and replays them through the public `Room` API:

* `Restore` recreates the room from the `create` record with the same
  deterministic `randFor("room:"+id)` stream (same board/deck as the original).
* `Join` / `Leave` / `PlayMove` are replayed in log order. Moves are deduped
  via `move_id`, joins via idempotency, so replaying the same file twice
  (or a file that contains a duplicate due to a retry that raced with a crash)
  is safe.

After replay `manager.SetWAL(store)` wires the store for future write-ahead.
`List` and `Get` see the rebuilt rooms; the first WebSocket connection for a
replayed match creates its `matchHub` lazily, so no hub state needs to be
persisted.

## Crash-recovery guarantee

Kill the process at any point after a client has received an ack. On restart
with the same `WALDir` and `Seed`:

* every acked create/join/leave/move is present,
* no rejected or duplicate move appears twice,
* `Snapshot` for any viewer is byte-identical,
* the game can continue and new moves are appended after the replayed prefix.

A crash between `WAL` (durable) and the future SQLite tier (B5) still recovers
because the WAL remains the source of truth until a checkpoint truncates it.

## Configuration

```bash
TESSERA_WAL_DIR=data/wal      # empty = disabled (tests)
TESSERA_WAL_FSYNC=always      # always|off
```

`data/wal` is ignored by `.gitignore`; tests use `t.TempDir()`.

## Layering

```
engine ← room (defines WAL interface)
wal → room, engine  (implements WAL, replays)
manager → WAL (via interface, no import of wal)
transport → room
cmd/tessera → wal, room, transport, config
```

`room` never imports `wal`; `go list -deps` still shows `room` importing
only `engine` + stdlib.
