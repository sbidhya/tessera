# B6 — Identity, matchmaking, and presence

`backend/internal/auth` mints anonymous credentials, `backend/internal/match`
pairs waiting players into rooms and tracks who is online, and
`backend/internal/transport` exposes both over HTTP. Layering stays
inward-pointing: `engine ← room ← {auth, match} ← transport`. `auth` is a
leaf (standard library only); `match` imports `engine` and `room` and nothing
else — no HTTP, no storage.

## Anonymous identity + lightweight token

There are no accounts and no user database. `POST /v1/players` mints a random
`p_`-prefixed player id (48 bits from the `"player-ids"` RNG stream, same
strength as room ids) and a token: `playerID.hex(HMAC-SHA256(secret,
playerID))`.

Why HMAC instead of a session table:

- **Stateless verification.** No token store, nothing to persist, nothing to
  look up per request. The check is one hash and a constant-time compare.
- **Identity survives restarts.** Seats in the WAL are keyed by player id and
  the secret is stable (env or seed-derived), so a token issued before a crash
  still verifies after recovery and the client still owns its seat.
- **No personal data.** Losing the token means losing the identity — there is
  no recovery flow, by design. The mobile client stores both strings in secure
  storage (M2).

The trade is explicit: **whoever holds the secret can mint any identity.**
The dev default derives the secret from the process seed
(`"auth-secret"` stream) so restarts keep working; the process logs a warning,
and production must set `TESSERA_AUTH_SECRET`. This is documented on the
config field, not hidden.

Until B6, `player_id` was a self-asserted dev identity (see `protocol.md`).
With the identity layer enabled, transport enforces tokens on every private
surface: `GET state?player_id=`, the WebSocket upgrade (checked *before*
`Accept` so rejections keep an HTTP status), `POST /v1/matches`, and the
matchmaking routes. Spectator state (`GET` without `player_id`) and match
listings stay public. With the layer disabled (`transport.Deps` zero value),
the server behaves exactly as in B3 — the legacy tests pin this.

## Matchmaking queue

`match.Matchmaker` is an actor, like `room`: one goroutine owns the waiting
queue, fed by a command channel. Pairing two waiters into a room is therefore
atomic with respect to cancels — there is no interleaving in which a player is
both paired and dequeued.

- **Blocking join, no tickets.** `POST /v1/matchmaking/join` stays open until
  a partner arrives. The request context *is* the queue membership: disconnect
  and the player is withdrawn. Clients set their own timeout (≈30s) and retry;
  a retry while still queued **attaches to the existing entry** instead of
  queueing twice, and an explicit `POST /v1/matchmaking/leave` withdraws
  without pairing (the still-open long-poll then ends with `204`, not an
  error).
- **Compatibility buckets.** Only waiters with equal `sequences_to_win`
  (normalized: 0 → 2, negative → 422) are paired, FIFO within a bucket. A
  quick test game never absorbs a full-match seeker.
- **Pairing seats both players.** The room is created through `room.Manager`
  (so it gets the same WAL record and replay as a directly created match) and
  both seats are joined before either waiter is released — a paired client
  always observes an already-started match. Pairing does bounded
  (`10s`-timeout) room calls so one wedged room cannot stall the lobby.
- **Cancel/paired race.** If the context ends in the same instant a partner is
  found, the match is real and the player is seated. A bounded ring (64) of
  recent pairing results lets the cancel path hand the client its match
  instead of stranding a ghost seat it can never learn about.

The queue is **in-memory by design** (the B6 brief says so): a crash drops
waiters and clients re-queue with backoff. Durability starts at pairing, where
the room's WAL takes over. `Close` rejects every waiter with a stable error
and is what the process calls before `Manager.Shutdown`.

## Presence

Two different questions, two different owners:

- *Is this seat's occupant connected to this match?* — the room's per-seat
  `Present` flag (existing).
- *Is this player connected at all?* — `match.Presence`, a refcounted online
  set fed by the transport hubs.

Refcounting matters because one player can hold sockets in several matches;
only the last disconnect takes them offline. A reconnect that replaces an
older socket does not move the count (the id never went offline), and hub
shutdown takes all its players offline so counts cannot leak. Presence is a
plain mutex-guarded map — transitions happen once per socket lifetime, not
once per move, so an actor would be ceremony without benefit (the same
reasoning `room.Manager` documents for its directory lock).

`GET /v1/presence` reports the online count; `GET /v1/presence/{player_id}`
reports one player. No heartbeats in B6: socket lifetime is the signal, which
is exact for "connected" and honest about what it does not cover (a player
with the app open but no socket is offline).

## What remains outside B6

Token expiry/revocation/scopes, guest display names, skill-based or
party-based matchmaking, cross-process presence (Redis, later), and
forfeit-on-timeout are all later blocks. The seams are in place: auth is a
two-method interface-shaped struct, the matchmaker constructs rooms through
the manager, and presence is a standalone tracker any pub-sub can replace.
