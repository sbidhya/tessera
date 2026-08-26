# B3 — Transport design notes

`backend/internal/transport` is the HTTP + WebSocket surface in front of the
in-memory room manager. It owns no game state; it translates HTTP/WS into
`room.Manager` / `room.Room` commands and translates room results back into
JSON. The layering stays inward-pointing:

```
engine  ←  room  ←  transport  ←  persistence  ←  infra
(pure)     (actor)  (HTTP+WS)
```

`transport` imports `engine`, `room`, `config` (for the `RandFunc` type) and
`gorilla/websocket` plus the standard library — nothing else. Persistence
(WAL, SQLite) will depend on `transport`/`room`, never the reverse, and
`engine` still imports only stdlib.

## Why this block exists

B0 proved the process boots and answers `/healthz`. B1 proved the rules are
correct. B2 proved a single match can be driven safely from many goroutines
with exactly-once move semantics. None of those were reachable from the
network. B3 makes the game playable from outside the process: browsers,
Flutter, and `curl` can create matches, list them, join them, fetch a
per-viewer snapshot for reconnect, and play a full game over WebSocket with an
authoritative server that broadcasts results.

## REST surface

All endpoints speak JSON and share the same `Manager`:

| Method | Path | Body / Query | Result |
|--------|------|--------------|--------|
| `GET`  | `/healthz` , `/api/healthz` | — | `{status, uptime}` |
| `POST` | `/matches` , `/api/matches` | `{num_players?, sequences_to_win?}` | `201 {room_id, seq, status}` |
| `GET`  | `/matches` , `/api/matches` | — | `{rooms: [{room_id, seq, status, players}]}` sorted by `room_id` |
| `GET`  | `/matches/{id}` , `/api/matches/{id}` | `?player_id=` (or `?viewer=`) | per-viewer `SnapshotDTO` |
| `POST` | `/matches/{id}/join` , `/api/matches/{id}/join` | `{player_id}` or `?player_id=` | `{seat, rejoined, seq, status}` |
| `GET`  | `/matches/{id}/ws` , `/api/matches/{id}/ws` | `?player_id=` | WebSocket upgrade |

Both `/matches` and `/api/matches` are accepted so the Flutter lobby (M2)
can use the conventional `/api` prefix while `curl` examples stay short.
Errors are `{"code", "message"}` with an HTTP status that mirrors the room /
engine sentinel that caused it (`400` for illegal moves, `404` for unknown
rooms, `409` for `stale_seq` / `room_full`, `410` for closed rooms).

### Snapshot privacy

`GET /matches/{id}?player_id=alice` renders `room.Snapshot(alice)` through
`snapshotToDTO`: `alice` sees her own `hand`, everyone else's sizes appear only
in `hand_counts`, and `Viewer` is her seat (or `NoPlayer` for a spectator).
Hiding happens in `room` and `dto.go`, not in the handler, so no future
endpoint can leak a hand by forgetting to redact. `GET` without a `player_id`
returns the spectator view. The same privacy holds on the WebSocket broadcast.

### Seeded creation

`POST /matches` delegates to `Manager.Create`, which draws the room id from a
`room-ids` RNG stream and the board/deck from a `room:<id>` stream, both
derived from `config.Config.Seed`. So one process seed reproduces both ids and
deals across restarts, while no two rooms share a shuffle.

## WebSocket protocol

Every frame is a typed envelope:

```json
{"type": "move", "seq": 0, "payload": { "move_id":"...", "expected_seq": 5, "type":"place", "card":{"rank":"A","suit":"Spades"}, "cell":{"row":1,"col":2} }}
```

`type` dispatches the payload, `seq` is the room's monotonic `Seq` after
the message's effect (or the current `Seq` for errors), and `payload` is
type-specific. The spec comes from `project.prompt` B3: `{type, seq, payload}`.

**Client → server**

* `move` — `MovePayload{move_id, expected_seq, type, card, cell}` where `type`
  is `place` | `remove` | `dead_card`. `move_id` is mandatory (idempotency);
  `expected_seq` is optional optimistic concurrency (`0` means "apply against
  whatever is current").
* `ping` — health check; server replies `pong`.

**Server → client**

* `state` — per-viewer `SnapshotDTO` (authoritative, privacy-preserving);
  broadcast after every accepted state change (join, leave, applied move) so a
  client that missed one broadcast can still catch up on the next.
* `move_result` — `MoveResultDTO{seq, duplicate, status, turn, winner}`; sent
  to the mover on every accepted move (with `duplicate:true` on a retry).
* `error` — `ErrorDTO{code, message}` with the room seq for context;
  `code` is the sentinel name (`not_your_turn`, `stale_seq`, `cell_occupied`,
  …).
* `pong` — reply to `ping`.

### Per-connection goroutine

Each WebSocket is one HTTP handler goroutine reading (`ReadJSON` loop) plus one
dedicated writer goroutine draining a buffered `send` channel (`writeLoop`).
That satisfies `project.prompt` "per-connection goroutine; client sends move
requests, server is authoritative and broadcasts results" without sharing
mutable state across connections.

### Hub (room → sockets)

A `hubRegistry` maps `roomID → set{*wsClient}` under a mutex. `Manager` owns
the game, the hub owns the set of observers; they meet only in `Server` which
wires `Room.PlayMove → hub.broadcastState`. `broadcastState` snapshots per
viewer so hands stay private, and skips a broadcast on `Duplicate:true` retries
(the room's `Seq` did not change). The hub's `send` channels are buffered (32)
so one slow client cannot stall the match.

### Idempotency, authority, and ordering

* The acting seat comes from the room's seating table, never the payload
  (`MoveRequest.PlayerID` is the `?player_id=` that was used to join the WS).
* `move_id` scoping is per `(player, move_id)` in `room`, so two clients cannot
  collide.
* The duplicate check runs before `ExpectedSeq` / rules checks, so a late retry
  replay returns the original `move_result` instead of a spurious `stale_seq`.
* `Seq` is the single total order for the match: the mailbox order in `room`
  is the order `transport` broadcasts and that B4 will append to the WAL.

### Reconnection

`Join` is idempotent, so a dropped socket reconnects by `GET /matches/{id}/ws?
player_id=same`. The handler re-joins (no state change, no seq bump), adds the
new `wsClient` to the hub, broadcasts the current state to all observers, and
the returning client receives a fresh per-viewer `state`. The required gate
flow — drop a socket mid-game, `GET /matches/{id}?player_id=` to fetch state,
re-`dial` and resume — is therefore just "re-Join".

## Concurrency and determinism

* The room's actor model still owns `GameState`; transport never touches it
  directly.
* The hub mutex guards only the directory of sockets (once per connection), not
  per move.
* `go test -race` covers every transport test, including the concurrent
  `32 × duplicate submit` and the `List` + `Create` hammer from B2 plus the new
  hub `50 × concurrent add` test.
* Determinism is preserved: the same `Seed` + the same sequence of REST/WS
  moves yields the same `Snapshot.Seq` order on every run, which is what B4's
  WAL replay will replay.

## Not in this block

No WAL or replay (B4), no SQLite write-behind (B5), no matchmaking or auth
(B6), no Redis / horizontal scaling / protobuf / metrics. The `Room` is still
closed only via `Manager.Close` / `Manager.Shutdown`; timed forfeit and
presence are policy for later.

## Wiring

`cmd/tessera/main.go` creates one `room.Manager` from `config.Config.NewRand`
and passes it to `transport.New` (with the process `start` time for `/healthz`
uptime). `transport.New(...).Handler()` is the `http.Server`'s handler; on
`SIGINT`/`SIGTERM` the server shuts down and `Manager.Shutdown()` closes every
room. `newRouter` (B0 healthz) is retained for the existing `TestHealthz*`
unit tests; production uses `newHandler` → `transport.New`.
