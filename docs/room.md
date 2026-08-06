# B2 — Room manager design notes

`backend/internal/room` is the first layer outside the engine. It owns the
*live* state of matches: which players are seated, whose turn it is, and the
`engine.GameState` itself. It imports only `internal/engine` and `internal/config`
(a leaf), so the inward-pointing layering still holds — `go list -deps` on the
package shows no transport or persistence.

## One goroutine per room (the actor model)

Each `Room` owns exactly one match's `*engine.GameState`, and exactly one
goroutine — the room's `run` loop — is allowed to touch it. Callers never get a
pointer to the state; they push a `command` onto a channel and wait on a reply
channel. There is no mutex anywhere on the gameplay path: mutual exclusion falls
out of the fact that only one goroutine is ever inside the loop.

Why not just hang a `sync.Mutex` off `GameState`?

- **Correctness by construction.** A mutex protects whatever the programmer
  remembers to lock, and the compiler will not remind them. Channel ownership
  makes it *structurally* impossible to read or write the state from the wrong
  goroutine, because no pointer to it ever escapes the room. `Snapshot`
  deep-copies for exactly this reason — see below.
- **It gives us a serialization point.** Every accepted command flows through
  one ordered queue. That is precisely the hook the write-ahead log (B4) needs:
  append to the log, then apply, then ack, in a single already-serialized place.
  Retrofitting that onto a mutex-guarded struct means finding every call site.

The cost is that a room handles commands strictly one at a time. That is the
right trade here: a room is one 2-player match and applying a move is
microseconds of pure computation. Rooms are fully independent, so throughput
scales with the number of rooms, not with concurrency inside one.

The registry (`Manager`) *does* use a mutex, deliberately. Creating, finding,
and reaping rooms is a cold path of a few map operations; the lock is never held
while a command is sent to a room, so one wedged match can never block match
creation elsewhere. Locks on the cold path, channels on the hot path.

### Reply channels are buffered

`do` allocates a `chan reply` with capacity 1 for every command. If it were
unbuffered and the caller's context expired while the command was being handled,
the run goroutine would block forever trying to deliver a reply nobody is
listening for — one dead client would wedge the entire match. With capacity 1
the room always hands off the reply and moves on.

`do` also selects on room shutdown at both steps (send and receive), so a caller
can never block on a room that has stopped.

## Snapshots are deep copies

`Snapshot` is the only way state leaves a room, and it copies the chips map, the
hands, the sequences, and the seat table. This is not an optimization detail —
it is what keeps the actor's invariant true. Handing back the live maps would let
a caller read them while the run goroutine writes, which is exactly the race the
design exists to prevent. `TestConcurrentRetriesAndReaders` runs four readers
iterating snapshot maps while a full game is played, under `-race`, to prove it.

The `*engine.Board` pointer *is* shared, because a board is immutable after
`engine.NewBoard` returns.

Two deliberate choices about what a snapshot contains:

- **Every hand is included.** The room is the authoritative server-side view;
  hiding information from ourselves buys nothing. Redacting opponents' hands is
  a per-player concern and belongs to the transport layer (B3).
- **The draw pile is a count, never the cards.** The order of undealt cards is
  the one piece of state no client may ever see, so it does not leave the room
  at all.

## Idempotency: move ids and expected seq

Networks deliver at-least-once. A client that times out and retries a move must
not place two chips. Two independent mechanisms cover this:

**`MoveID` (mandatory).** The room keeps a table of accepted move ids and
replays the original acknowledgement for a duplicate instead of applying it
again. Three details matter:

1. The duplicate check runs *before* every other validation. By the time a retry
   arrives the turn has usually passed and the target cell is occupied — both of
   which are rule violations for a *new* move. Checking the dedupe table first is
   what stops a dropped ack from turning a legal move into a spurious rejection.
2. Only **accepted** moves are recorded. A rejected move had no effect, so a
   retry should be re-judged rather than served a cached rejection — a client
   correcting a stale `ExpectedSeq` reuses the same id and expects it to work.
   This also keeps the table identical to the set of commands B4 writes to the
   log.
3. Ids are scoped per player (`player\x00move_id`), so two clients numbering
   their moves 1, 2, 3, … can never collide.

**`ExpectedSeq` (optional).** `Seq` is the game state version: 1 after the deal,
+1 per applied move. A client may pin a move to the version it was looking at;
a mismatch returns `ErrStaleSeq` rather than applying the move to a board the
player never saw. Zero means "unchecked", which is unambiguous because a live
game always has `Seq >= 1`.

"Expected turn" needs no separate field: `engine.Apply` already rejects a move
whose player is not the player to move, and that `engine.ErrNotYourTurn` passes
through the room unwrapped.

### Presence does not bump `Seq`

Join and Leave change the seat table but **not** the game version. If a
reconnect bumped `Seq`, it would invalidate a legal move another player already
had in flight — a presence event should never cost someone their turn.

## Seats, joining, and leaving

The deck is dealt at room construction, before anyone joins, and the room starts
in `StatusWaiting`; it flips to `StatusPlaying` when the last seat fills.
Dealing eagerly means room creation is the only place that can fail (bad player
count), and the deal stays a pure function of `(seed, room id)`.

- **Join is idempotent.** Joining with a `PlayerID` that already holds a seat is
  a reconnect: the same seat comes back with `Reconnect: true`. A retried join
  must not silently consume the second seat.
- **Leave before the game starts frees the seat** — nothing has been played, so
  it carries no history.
- **Leave during play holds the seat** and only clears presence. A match with an
  empty seat is unresumable, and B3's reconnect path depends on the same
  `PlayerID` finding its hand exactly where it left it.

## Reproducibility

`Manager` names rooms with a monotonic counter (`room-1`, `room-2`, …) and seeds
each with `config.NewRand(id)`. Two properties follow: the whole process replays
from one integer seed, and each room gets a statistically independent stream, so
concurrent shuffles never alias. `TestManyRoomsInParallel` asserts the
independence; `TestGameIsDeterministic` asserts that the same seed plus the same
commands yields byte-identical final state — the property B4's replay is built
on.

## Test strategy

The gate is "drive a full 2-player game in-process, and pass `-race` under
load", so the tests play real games rather than poking at internals:

- A small deterministic bot (`bot_test.go`) decides moves from a `Snapshot`
  alone — the same surface a real client gets in B3. It maximizes the longest
  run through a candidate cell, which keeps games short (25–41 moves) and
  bounded.
- `driveGame` plays a match through `Join` → `PlayMove` → win, asserting the
  version advances by exactly one per accepted command.
- `TestConcurrentRetriesAndReaders` replays the same game with every move
  submitted by 8 goroutines at once (a retry storm) plus 4 continuous snapshot
  readers, and asserts exactly one apply per move id.
- `TestManyRoomsInParallel` runs 12 independent matches concurrently through the
  manager.
- `TestDeadCardSwapKeepsTurn` is scripted rather than dealt, because a dealt
  game only produces a dead card deep into a clogged board.
