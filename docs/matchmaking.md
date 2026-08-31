# B6 — identity, matchmaking, and presence

## Why this is a separate layer

The room actor still owns all match state and accepts a player id, not a token.
Authentication and pairing live in `internal/match`, outside the room package,
so the game engine and room manager remain independent of HTTP and credential
formats. Transport verifies a token, obtains its player id, and only then calls
the room.

## Anonymous identity

`POST /v1/auth/anonymous` creates a 128-bit player id from `crypto/rand` and an
HMAC-SHA256 bearer token. Game randomness remains seeded and reproducible;
credential entropy intentionally is not predictable from `TESSERA_SEED`.

Tokens are stateless and survive restarts when `TESSERA_AUTH_SECRET` is stable.
The built-in secret is for local development only. This light-auth block has no
account recovery, expiry, revocation, or user profile.

## Matchmaking

The service keeps one FIFO queue per `sequences_to_win` value. The second
compatible player causes a two-seat room to be created and both identities to
be seated through the existing room API. It then marks both seats disconnected:
matchmaking reserves seats, while WebSocket connections own presence.

Queue metadata is intentionally in memory. Durable game state starts once the
room is created and continues through the existing WAL, but waiting queues and
the player-to-match lookup reset when the process restarts.

## Presence

Presence is a connection count, not a single boolean assignment. This handles
socket replacement and a player connected to more than one room without an old
socket incorrectly marking the player offline. Room snapshots continue to show
per-match presence; `/v1/presence/{player_id}` reports process-wide presence.
