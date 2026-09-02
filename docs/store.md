# B5 — SQLite cold tier and write-behind

Finished matches now move from the live room/WAL tier into SQLite. SQLite stores
the match result, its accepted event history, each participant's result, and
aggregate player statistics. In-progress matches remain only in memory plus the
WAL, where they can be replayed after a crash.

## Durability order

The winning move still follows B4's append-before-apply rule. Once the room has
published that terminal state, it enqueues a `FinishedMatch` value to the cold
store; the room never performs SQL itself.

```text
winning move -> fsync WAL -> publish finished room -> enqueue archive -> ack
                                                   |
                                                   v
                       batch SQLite transaction -> commit -> checkpoint WAL
                                                       |
                                                       v
                                 start retention window -> evict room + hub
```

This ordering deliberately leaves two safe crash outcomes:

- A crash before the SQLite commit leaves the complete WAL. Startup replays the
  finished room and queues the archive again.
- A crash after the SQLite commit but before the WAL checkpoint leaves both
  copies. The match id makes the repeated SQL transaction a no-op, so player
  statistics are not counted twice; checkpointing then finishes normally.

The successful SQLite commit acknowledges archival and starts the room's
retention window. A WAL checkpoint failure is retried but does not keep the
already-archived live room resident forever.

The WAL is never truncated first. A checkpoint also verifies that the requested
terminal sequence is the file's final event, preventing an unarchived suffix
from being discarded. Finished rooms stop recording presence changes so the
winning move remains that final event.

## Write-behind worker

`internal/store` owns one goroutine and one SQLite connection. Rooms submit
completed values to its buffered queue. The worker flushes when either the batch
size is reached or the flush interval expires, and a graceful shutdown drains
the remaining queue once.

One SQLite transaction writes every match in a batch:

- `matches` — final sequence, options, winner, move count, archive time;
- `match_players` — seat, result, and completed-sequence count;
- `match_events` — the accepted room events in sequence order as JSON;
- `player_stats` — matches played, wins, losses, and sequences completed.

`matches.id` is the idempotency boundary. Stats are updated only when that row
is first inserted, so recovery and checkpoint retries cannot double-count them.
An SHA-256 digest of the complete terminal projection also makes a conflicting
reuse of a match id fail rather than silently checkpointing different history.
SQLite itself runs in WAL journal mode with full synchronous commits; this is
separate from Tessera's per-match recovery WAL.

The pure engine and room actor remain inward layers. `room` defines only the
persistence-neutral finished-match value and sink interface; the concrete SQL
driver and schema live in `internal/store`.

## Configuration

- `TESSERA_DB_PATH` — SQLite file; default `data/tessera.db`.
- `TESSERA_STORE_BATCH_SIZE` — maximum matches per transaction; default `16`.
- `TESSERA_STORE_FLUSH_INTERVAL` — maximum wait for a partial batch, as a Go
  duration; default `1s`.
- `TESSERA_FINISHED_MATCH_RETENTION` — reconnect grace window after archival;
  default `5m`. When it expires, the room actor and transport hub stop and the
  in-memory idempotency map and event history are released.

The implementation uses `modernc.org/sqlite`, a pure-Go `database/sql` driver,
so builds do not require a C compiler or a system SQLite installation.

## Test gate

From `backend/`:

```sh
go test ./...
go test -race ./...
go vet ./...
```

`TestCrashBetweenWALAndSQLiteRecovers` runs the crash window end to end. The
store tests also prove commit-before-checkpoint ordering, batch aggregation,
history persistence, and exactly-once stats across checkpoint retries.
