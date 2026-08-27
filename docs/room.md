# B2 — Room manager design notes

`backend/internal/room` is the layer between the pure engine and everything that
talks to a network. It owns *live* matches: one `Room` per match, holding that
match's authoritative `engine.GameState`, plus a `Manager` that is the process's
directory of rooms.

It imports `engine` and the standard library — nothing else. Transport and the
concrete WAL depend on this package, never the reverse. The room exposes a tiny
`EventJournal` interface so it can enforce append-before-apply without knowing
anything about files or databases.

## One goroutine per match (the actor model)

Every room's state is mutated by exactly **one** goroutine — the room's own loop
— fed by a buffered channel of commands (its "mailbox"). Callers on any other
goroutine send a command and wait for a reply; they never touch the
`GameState`.

```
caller ──command{args, replyChan}──▶ [ mailbox ] ──▶ room goroutine ──▶ GameState
       ◀───────── reply ─────────────────────────────┘
```

A mutex around `GameState` would also be correct. The actor shape was chosen for
three reasons that matter specifically for a game server:

- **Serialisation comes for free.** "Is it your turn?" and "apply the move" are
  one indivisible step because no other move can be in flight. There is no lock
  for a future contributor to forget to hold, and no window in which two players
  both pass validation.
- **The mailbox order *is* the match's history.** There is exactly one ordering
  of events per match, decided in one place. That is precisely what the WAL (B4)
  must append and what the WebSocket layer (B3) must broadcast; both become
  "hook into the loop" rather than "reconstruct an order after the fact".
- **Isolation is cheap.** Rooms share nothing, so matches scale across cores
  with zero contention and one wedged match cannot stall another.

The price is discipline: anything handed back to a caller must be a **copy**, or
the "one goroutine owns the state" invariant leaks and we are back to shared
memory without the lock. `Snapshot` deep-copies every map and slice; the
`-race` tests exist to catch a regression there. The `Board` is the one shared
pointer — it is immutable after construction.

`Manager` does use a `sync.Mutex`, which is not a contradiction: it guards the
*directory* (create/lookup/close), touched once per match rather than once per
move. The hot path stays lock-free.

## Idempotency: move_id, and why the order of checks matters

Mobile clients retry. A dropped ack is indistinguishable, from the client's
side, from a dropped move — so a client that times out **must** resend, and the
server must not place a second chip. Every `MoveRequest` therefore carries a
client-generated `MoveID`, and the room remembers the result of each *accepted*
move keyed by `(player, move_id)`. A resend replays the original ack with
`Duplicate: true` instead of re-applying the move.

Two details carry most of the weight:

- **The duplicate check runs first**, before the game-started, staleness and
  rules checks. A retry can legitimately arrive after the opponent has moved or
  after the match has ended; answering it with `ErrStaleSeq` or `ErrGameOver`
  would turn a *successful* move into a spurious failure purely because an ack
  was lost. `TestDuplicateAckSurvivesLaterState` pins this.
- **Rejected moves do not consume their id.** An illegal move had no effect, so
  the id stays free and a corrected retry is evaluated normally. Only accepted
  moves are recorded.

Keys are scoped per player so two clients cannot collide on the same generated
id. The map is bounded by the number of accepted moves in one match (bounded by
the deck), so no eviction is needed at this scale.

## Sequence numbers and optimistic concurrency

Each room keeps a monotonic `Seq`, bumped on every accepted state change (join,
leave, applied move, match start). A client holding `Seq = N` has seen
everything up to N.

`MoveRequest.ExpectedSeq` is optional optimistic concurrency control: if
non-zero, the move is rejected with `ErrStaleSeq` unless the room is still at
that version — "I am acting on exactly the state I last saw". Zero means "apply
against whatever is current", which is what a client should send when it is
happy to let the engine's turn check arbitrate.

`ExpectedSeq` subsumes the "expected turn" idea from the project brief: the turn
is part of the state that `Seq` versions, and a stricter check costs nothing
when it is opt-in. B3 uses the same counter to order broadcasts.

## Seats, leaving, and reconnection

Players are identified by an opaque string id (real identity arrives in B6). The
room maps player id → engine seat, and **Join is idempotent**: a player already
in the room always gets their existing seat back. Joining while already present
is a no-op; rejoining after a disconnect changes `Present` from false to true
and therefore bumps `Seq` once. Reconnection is still just "Join again", while
every observable presence state keeps a distinct version.

`Leave` is deliberately asymmetric:

- **Before the match starts**, the seat is released for someone else.
- **Once it is under way**, the seat is held and the player marked absent. One
  dropped socket must not forfeit a match; forfeit-on-timeout is a policy
  decision for a later block, not an accident of TCP.

The match begins the moment the last seat is filled. Cards are dealt *at room
construction*, not at start, so invalid `engine.Options` fail in exactly one
place — `New` — and a `Room` that exists always holds a valid `GameState`. Play
is gated on `Status`, not on whether the deal has happened.

## Server authority and hidden information

Two properties are enforced here rather than left to the transport layer, so no
future endpoint can forget them:

- **The acting seat comes from the room's seating table, never from the
  request.** A client cannot move on another player's behalf even by replaying a
  valid payload (`TestPlayerCannotSpoofAnotherSeat`).
- **`Snapshot` is per-viewer.** You see your own hand; for everyone else you get
  only a card count. An unknown or empty viewer id gets the spectator view.

## Determinism

`Manager` takes a `RandFunc` — satisfied by `config.Config.NewRand` — and derives
two PCG seed words from each room's named stream (`"room:"+id`), with room ids
themselves drawn from a `"room-ids"` stream. Those room seed words are stored in
the creation event, so an existing match replays identically even if the process
seed later changes. No two rooms share a shuffle
(`TestManagerIsDeterministic`, `TestManagerRoomsHaveIndependentStreams`).

Room ids are random rather than sequential: until real auth lands in B6, knowing
a room id is effectively the capability to join that match.

## Shutdown

`Room.Close` closes a `quit` channel; the loop exits and closes `done`. Every
caller waiting on a reply also selects on `done` (and on its own
`context.Done()`), so shutting a room down **fails** in-flight callers with
`ErrRoomClosed` rather than stranding them — the property
`TestCloseUnblocksWaitingCallers` checks. Reply channels are buffered so the room
never blocks handing a result to a caller that has already walked away on
context cancellation.

## What remains outside the room layer

HTTP/WebSocket concerns stay in `internal/transport`; filesystem framing and
sync policy stay in `internal/wal`; the SQLite cold tier stays in B5; and
matchmaking/auth stay in B6. The room package knows only its persistence port
and durable event values, and continues to import only the engine and standard
library. See `docs/wal.md` for the B4 ordering and recovery design.
