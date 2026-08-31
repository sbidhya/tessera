# B3 — HTTP and WebSocket protocol

The server is authoritative: clients send an intended move, and only the room
actor decides whether it applies. Transport never edits game state. Accepted
moves are serialized by one transport hub per match and broadcast in increasing
room `seq` order.

All HTTP responses and WebSocket payloads are JSON. The examples omit some
state fields for brevity.

## REST

### Create a match

```http
POST /v1/matches
Content-Type: application/json

{"sequences_to_win": 1, "player_id": "p_...", "token": "p_...."}
```

`sequences_to_win` defaults to `2`. B3 is fixed to the v1 scope of two players.
With the identity layer enabled, `player_id` + `token` are required; without
it they are accepted but ignored.

```json
{
  "match": {
    "id": "r_12ab34cd56ef",
    "seq": 1,
    "status": "waiting",
    "players": 0,
    "present": 0,
    "capacity": 2,
    "sequences_to_win": 1
  }
}
```

### List matches

```http
GET /v1/matches
```

Returns `{"matches": [...]}` with the same summary shape as create.

### Get authoritative state

```http
GET /v1/matches/{match_id}?player_id=alice
```

The optional `player_id` selects a private view. A seated player receives only
their own `hand`; an omitted or unknown id gets a spectator view with an empty
hand. Every view contains public board cells, chips, sequence lines, per-seat
hand counts, turn, winner, players/presence, and remaining draw count.

With the B6 identity layer enabled, `player_id` must be accompanied by its
`token` (`?player_id=...&token=...` on GET/WebSocket, `{"player_id",
"token"}` in POST bodies). Without the layer, any `player_id` is accepted as
before. The spectator view (no `player_id`) never needs a token.

## WebSocket

Connect with:

```text
GET /v1/matches/{match_id}/ws?player_id=alice&token=alice-token
```

Opening the socket joins the room. Reopening it with the same id restores the
same seat. A newer socket for the same player replaces the old one.

Every message uses one envelope:

```json
{"type": "state", "seq": 3, "payload": {}}
```

For server messages, `seq` is the room version described by the message. For a
client move it is optimistic concurrency control: the move applies only if the
room is still at that version. Sending `0` opts out and relies on normal turn and
rule validation.

### Client → server: move

Normal card or two-eyed jack placement:

```json
{
  "type": "move",
  "seq": 3,
  "payload": {
    "move_id": "phone-7",
    "move": "place",
    "card": {"rank": "A", "suit": "spades"},
    "cell": {"row": 4, "col": 6}
  }
}
```

One-eyed jack removal uses `"move": "remove"`. A dead-card exchange uses
`"move": "dead_card"` and omits `cell`.

Ranks are `A`, `2` … `10`, `J`, `Q`, `K`; suits are `spades`, `hearts`,
`diamonds`, `clubs`. `move_id` must be reused when retrying the same intended
move. Accepted retries return the original result with `duplicate: true` and do
not mutate or rebroadcast state.

### Server → client: move result

Every newly accepted move is broadcast to both players before its resulting
state:

```json
{
  "type": "move_result",
  "seq": 4,
  "payload": {
    "move_id": "phone-7",
    "player_id": "alice",
    "duplicate": false,
    "status": "playing",
    "turn": 1,
    "winner": null
  }
}
```

### Server → client: state

`state` is sent on connect, join/reconnect, disconnect, and after each accepted
move. Each connection gets a separately rendered snapshot, so an opponent's
cards never enter the socket's outbound queue.

### Server → client: error

Rejected moves do not change `seq`:

```json
{
  "type": "error",
  "seq": 4,
  "payload": {
    "code": "stale_seq",
    "message": "room: stale sequence number: room at 4, client expected 3"
  }
}
```

Stable codes cover membership (`room_full`, `not_seated`), concurrency
(`stale_seq`, `missing_move_id`), turn/rule failures (`not_your_turn`,
`card_not_in_hand`, `cell_occupied`, and related engine errors), malformed
messages, shutdown, and `durability_failure` when an accepted transition could
not be written safely. A durability failure leaves the live state unchanged.

## Reconnect flow

1. A dropped socket marks its held seat absent and advances `seq`.
2. The client calls `GET state?player_id=...` to recover the authoritative board,
   private hand, and latest `seq`.
3. It opens the WebSocket again with the same `player_id`; the room restores the
   same seat, marks it present, and broadcasts the new state.
4. The next move uses the recovered/broadcast `seq`.

The B3 integration test performs this flow mid-game and then plays through to a
winner with two actual WebSocket clients.

## B6 — Identity, matchmaking, presence

### Issue an identity

```http
POST /v1/players
```

```json
{"player_id": "p_9f2c…", "token": "p_9f2c….hmac…"}
```

Anonymous and unguessable. The client keeps both halves; the token is the
proof. No request body is needed. Answers `503 auth_disabled` when the server
runs without the identity layer.

### Matchmaking

```http
POST /v1/matchmaking/join
Content-Type: application/json

{"player_id": "p_9f2c…", "token": "p_9f2c….hmac…", "sequences_to_win": 1}
```

The call stays open until a partner with the same `sequences_to_win` is
found, then both callers receive:

```json
{"match_id": "r_12ab…", "seat": 0, "player_id": "p_9f2c…"}
```

Both seats are already joined, so the match is `playing` — open the sockets
and play. The request context is the queue membership: disconnect and you are
withdrawn. Set a client-side timeout (≈30s) and retry; a retry while still
queued attaches to the existing entry. If you left the queue via
`POST /v1/matchmaking/leave` while the long-poll was open, it ends with
`204` (no match), and the leave call itself answers `{"cancelled": true}`.
`GET /v1/matchmaking/status` answers `{"waiting": n}`.

### Presence

```http
GET /v1/presence
GET /v1/presence/{player_id}
```

```json
{"online": 2}
{"player_id": "p_9f2c…", "online": true}
```

Counts live WebSocket connections across all matches. A reconnect that
replaces a socket does not flap the count; the last disconnect takes the
player offline.

### Error codes (B6 additions)

`missing_token` / `invalid_token` (401), `auth_disabled`,
`matchmaking_disabled`, `presence_disabled` (503), `invalid_options` (422 for
a negative `sequences_to_win`). The B6 integration test pairs two anonymous
clients through the queue, plays a full game over real WebSockets with a
mid-game drop, recovers via token-authenticated `GET state`, reconnects with
the same identity, and finishes — with presence asserting each transition.
