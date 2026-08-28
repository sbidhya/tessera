# B4 — Write-ahead log and recovery

Tessera now records every accepted room state transition in a per-match WAL
before exposing that transition in memory or acknowledging it to a client. The
default server configuration uses `fsync` for every record.

## Layer boundary

`internal/room` defines the small `EventJournal` interface and the durable event
schema because the room actor is the authority that decides whether a command
is valid. `internal/wal` implements that interface with files and imports
`room`; the room package still imports only the engine and standard library.

For a move, the actor follows this order:

```text
validate against a cloned GameState
        -> append event (+ fsync under "always")
        -> publish the prepared GameState
        -> cache move_id result
        -> acknowledge/broadcast
```

Preparing the move on a clone matters. Validation cannot happen after the WAL
write because rejected commands must not be logged, and the live state cannot
be mutated before the write because a crash or disk error would make memory
newer than durable history. Once the append succeeds, assigning the already
validated clone cannot fail.

Joins and leaves use the same append-before-mutate rule. True no-ops—an already
present player joining again or a duplicate `move_id` retry—reuse their prior
answer without adding another event.

## Per-match files and record framing

Each match is stored as `<wal_dir>/<room_id>.wal`. A record contains:

```text
4-byte magic "TSW1" | uint32 payload length | uint32 CRC-32 | JSON event
```

The length makes records independently readable and the checksum detects
corruption. If a process dies during its final write, startup truncates that one
partial tail back to the last verified record. Bad magic, an invalid checksum,
an unknown event version, conflicting records at one sequence, or a sequence
gap fails startup rather than silently constructing a different match.

Files are mode `0600`; the containing directory is created as `0750`. Match
files have independent locks, so an `fsync` for one room does not serialize WAL
writes for every other room.

## Deterministic replay

The first record is `room_created` at sequence 1. It stores the normalized game
options and two PCG seed words used for that room. This makes an existing match
self-contained: changing the process-wide `TESSERA_SEED` after a restart does
not change its board, deal, or draw order.

Startup reads all WALs, reconstructs each initial game from its stored seed, and
applies joins, moves, and leaves in sequence order. Applied move ids and their
original results are rebuilt along the way, so a client retry after a crash is
still answered as a duplicate instead of applying twice. Exact duplicate WAL
records are ignored by sequence and value; a different event claiming the same
sequence is corruption.

Seats survive restart, but `Present` is reset to false because sockets are
process-local. A reconnecting player joins the same seat and creates the next
durable presence transition.

## Configuration

- `TESSERA_WAL_DIR` — WAL directory; default `data/wal`.
- `TESSERA_WAL_SYNC=always` — `fsync` each record before acknowledgement
  (default and production-safe behavior).
- `TESSERA_WAL_SYNC=never` — rely on OS flushing. Faster for experiments, but a
  machine failure can lose recently acknowledged events.

If a write or `fsync` fails, that match log is poisoned for the rest of the
process. The attempted state is not published, and clients receive
`durability_failure`. Restart performs checksum/tail recovery before accepting
new commands.

After B5 archives a finished match in SQLite, it calls `Checkpoint` with the
terminal sequence. The WAL verifies that no newer record exists, truncates the
file to zero, and rejects further appends for that finished room. SQLite commits
first, so a crash can leave an extra WAL to replay but can never leave neither
copy. See `docs/store.md` for the full write-behind protocol.

## Test gate

From `backend/`:

```sh
go test ./...
go test -race ./...
go vet ./...
```

`TestKilledProcessRecoversMatch` starts a subprocess, creates and advances a
match, exits it without closing the manager or WAL, then opens a fresh manager
and verifies the board, turn, sequence, seats, and duplicate move acknowledgement.
The WAL tests also cover torn-tail repair, checksum rejection, per-match files,
and continued appends after repair.
