# Tessera — build progress

Tracks which building blocks (from `project.prompt`) are done, so work resumes
from the right point. One block at a time; the next block starts only after the
current one is confirmed passing.

## Backend

- [x] **B0 — Skeleton & tooling** _(PR: b0-skeleton-tooling)_
  - Go module `github.com/sbidhya/tessera/backend`, repo layout, `backend/Makefile`.
  - `/healthz` HTTP endpoint (200 JSON `{status, uptime}`), structured `log/slog`
    JSON logging, graceful shutdown on SIGINT/SIGTERM.
  - `internal/config`: env-overridable config + **seeded RNG** (`Config.NewRand`,
    deterministic from a single int64 seed).
  - Gate: `curl /healthz` = 200 ✅; `go test ./...` clean ✅; `go test -race` clean ✅.
- [x] **B1 — Pure game engine (no I/O)** _(PR: b1-game-engine)_
  - `internal/engine`, standard-library only — the innermost layer imports no
    networking/DB (enforces the inward-pointing layering).
  - Model: `Card`/`Rank`/`Suit` + jack classification (two-eyed J♦/J♣ wild,
    one-eyed J♥/J♠ remove); 104-card double deck; seeded `Shuffle`.
  - `Board`: 10×10, four wild corners, cell↔card index. Layout is **generated
    deterministically from the injected RNG** (correct-by-construction: every
    non-jack card appears exactly twice) — a fixed canonical layout can be
    swapped into `NewBoard` later with zero rule changes. See `docs/engine.md`.
  - `GameState` + `Apply(Move)`: deal, place (normal + two-eyed jack),
    one-eyed-jack remove, dead-card swap (once/turn, keeps turn), draw,
    sequence detection (all 4 directions incl. corners, "reuse ≤1 cell" overlap
    rule), win at `SequencesToWin`. Validation is transactional: a rejected move
    leaves state unchanged.
  - Determinism: given the same injected `*rand.Rand` + moves, byte-identical
    state every run (needed for B4 WAL replay).
  - Gate: `go test ./...` clean ✅ (54 engine cases, 97.5% coverage);
    `go test -race ./...` clean ✅; `go vet` + `gofmt` clean ✅.
- [x] **B2 — In-memory room manager** _(PR: b2-room-manager)_
  - `internal/room`, importing `engine` + stdlib only (layering verified with
    `go list -deps`). Not yet wired into `cmd/tessera` — that lands with B3.
  - **Actor model**: one goroutine owns each match's `GameState`, fed by a
    buffered command mailbox (`Join`, `PlayMove`, `Leave`, `Snapshot`). No locks
    on the hot path; the mailbox order is the match's event order, which is what
    B3 broadcasts and B4 logs. `Manager` uses a mutex only for the room
    directory (once per match, not per move).
  - **Idempotency**: every move carries `move_id` + optional `ExpectedSeq`.
    Accepted moves are cached per `(player, move_id)`; a retry replays the
    original ack (`Duplicate: true`) instead of re-applying. The duplicate check
    runs *before* staleness/rules checks so a late retry can't be turned into a
    spurious error. Rejected moves don't consume their id.
  - **Authority & privacy**: the acting seat comes from the room's seating
    table, never the request; `Snapshot` is per-viewer (own hand only, deep
    copied) so no future endpoint can leak an opponent's cards.
  - **Reconnect**: `Join` is idempotent; mid-match `Leave` holds the seat.
  - Determinism: per-room RNG streams from one process seed (ids + deals
    reproducible, no two rooms share a shuffle). See `docs/room.md`.
  - Gate: full 2-player match driven in-process over 6 seeds ✅; concurrent-load
    test (32 simultaneous duplicate submits apply exactly once; both players
    hammering + snapshot readers + leave/join churn) ✅; `go test ./...` clean ✅;
    `go test -race ./...` clean over 5 runs ✅ (98.8% coverage); `go vet` +
    `gofmt` clean ✅.
- [x] **B3 — Transport (HTTP + WebSocket)** _(PR: b3-http-websocket-transport)_
  - `internal/transport` exposes REST create/list/state endpoints and a typed
    WebSocket envelope `{type, seq, payload}`. The process now wires the B2 room
    manager into `cmd/tessera`; `/healthz` remains unchanged.
  - One transport hub goroutine per match serializes socket join/disconnect/move
    handling and broadcasts, preserving room sequence order. Each connection
    has its own bounded writer queue; slow clients are disconnected rather than
    blocking the match.
  - Server authority and privacy carry through the wire: `player_id` maps to a
    held room seat, move `seq` provides optimistic concurrency, `move_id`
    retries replay the original ack, and state is rendered separately so each
    player receives only their own hand.
  - Reconnect: disconnect marks a held seat absent; `GET state?player_id=...`
    restores authoritative private state; reopening the socket restores the
    same seat and resumes play. See `docs/protocol.md`.
  - WebSockets use `github.com/coder/websocket`, the one justified dependency
    because the Go standard library does not implement the protocol.
  - Gate: two real WebSocket clients play a full game through an HTTP test
    server, including stale-seq rejection, duplicate retry, mid-game drop, REST
    recovery, reconnect, and resume ✅; `go test ./...` ✅; `go test -race
    -count=5 ./...` ✅; `go vet` + `gofmt` + inward-layer dependency check ✅.
- [x] **B4 — Durability: WAL + replay** _(PR: b4-wal-replay)_
  - `internal/wal` stores checksummed, length-framed events in one append-only
    file per match. `TESSERA_WAL_SYNC=always` (the default) fsyncs the file and
    new directory entry before acknowledgement; `never` is available for local
    throughput experiments.
  - The room actor owns the durability boundary through a small interface:
    accepted joins, moves, and leaves are appended before authoritative state is
    published. Moves are validated on a deep clone first, so a rejected command
    or WAL failure leaves live state untouched.
  - Creation records persist normalized options plus per-room PCG seed words.
    Startup reconstructs rooms independently of the current process seed,
    reapplies events in sequence order, rebuilds `move_id` acknowledgements, and
    marks recovered seats disconnected until clients rejoin.
  - Recovery accepts exact duplicate records idempotently, rejects sequence
    gaps/conflicts/corruption, and truncates an incomplete final frame left by a
    killed writer. See `docs/wal.md`.
  - Gate: subprocess exits mid-game without cleanup and a fresh manager recovers
    the board/turn/sequence plus duplicate ack ✅; `go test ./...` ✅; `go test
    -race ./...` ✅; `go vet` + `gofmt` + inward-layer dependency check ✅.
- [x] **B5 — Cold tier: SQLite + write-behind** _(PR: b5-sqlite-write-behind)_
  - `internal/store` batches finished matches into SQLite transactions using the
    pure-Go `modernc.org/sqlite` driver. The schema stores match summaries,
    participants, the accepted event history, and aggregate player stats.
  - The room layer emits a persistence-neutral terminal projection only after
    the winning move is durable and published. SQL stays outside the room actor;
    the write-behind worker flushes by batch size or interval and drains on
    graceful shutdown.
  - Exactly-once stats use `matches.id` as the transaction idempotency key. A
    retry after SQLite commit does not increment wins/losses twice.
  - WAL checkpointing happens only after the SQLite commit and verifies the
    archived sequence is the log's final event before truncating it. Recovered
    finished WALs are automatically queued again, covering crashes anywhere in
    the write-behind window. See `docs/store.md`.
  - Gate: history + stats persistence ✅; batch + checkpoint retry/idempotency
    tests ✅; subprocess crash between terminal WAL and SQLite recovers and then
    checkpoints ✅; `go test ./...` ✅; `go test -race ./...` ✅; `go vet` +
    `gofmt` + inward-layer dependency check ✅.
- [x] **B6 — Matchmaking, presence, light auth** _(PR: b6-matchmaking-presence-auth)_
  - `internal/auth`: anonymous `p_` identities + stateless HMAC-SHA256 tokens
    (`Issue`/`Verify`, constant-time compare). Leaf package, stdlib only.
    Secret from `TESSERA_AUTH_SECRET`, else a seed-derived dev default (warns);
    tokens therefore survive restarts and still own their WAL seats.
  - `internal/match`: actor-model `Matchmaker` (blocking join, FIFO pairing
    within equal-`sequences_to_win` buckets, both seats joined before release,
    idempotent re-join attaches, explicit leave → 204, cancel/paired race
    recovered via a bounded recent-pairing ring, `Close` unblocks waiters) and
    a refcounted `Presence` tracker (nil-safe, socket lifetime is the signal).
    Imports `engine` + `room` only.
  - `internal/transport`: `NewWithDeps` (plain `New` preserves B3 no-auth
    behavior); `POST /v1/players`, `POST /v1/matchmaking/join|leave`,
    `GET /v1/matchmaking/status`, `GET /v1/presence[/{player_id}]`; token
    enforcement on private state, WS upgrade, and match creation; hub
    presence hooks (replace doesn't flap, shutdown drains).
  - `cmd/tessera` wires and shuts down the lobby (`api` → `matchmaker` →
    `manager`); `docs/match.md` + `docs/protocol.md` document the design.
  - Gate: two anonymous clients matchmake, pair, play a full game over real
    WebSockets with a mid-game drop, recover via token `GET state`, reconnect
    with the same identity, and finish; presence asserted throughout ✅;
    `go test ./...` ✅; `go test -race ./...` ✅; `go vet` + `gofmt` +
    inward-layer dependency check ✅.

## Mobile (Flutter)

- [x] **M1 — App skeleton → `/healthz` status** _(PR: m1-server-status)_
  - Flutter 3.47 app (`mobile/`, iOS + Android only), `http` as the one
    dependency. `lib/server_health.dart` is a pure-Dart `GET /healthz` client
    (`ServerHealth` model + `fetchServerHealth`, injected `http.Client`,
    5s timeout, all failures mapped to `ServerHealthException`); later blocks
    reuse it as the connectivity probe.
  - `lib/health_screen.dart`: editable server URL (defaults to
    `http://10.0.2.2:8080` on Android — the emulator's host-loopback alias —
    `http://localhost:8080` on iOS simulator), checks on launch, green
    reachable card (`status`/`uptime`) or red unreachable card (reason).
  - Flutter SDK lives at `/tmp/sdks/flutter` (only writable spot on this
    machine; `/tmp` is cleared on reboot — reinstall to a permanent path).
  - Gate: `flutter test` 17/17 ✅ (client unit tests on a mock HTTP client +
    widget tests incl. in-flight disabled button); `flutter analyze` clean ✅;
    app's real client verified against a live backend (200 parsed; refused
    connection → clean error) ✅. Manual emulator play stays the gate for M2+.
- [x] **M2 — Identity + lobby** _(PR: m2-identity-lobby)_
  - `POST /v1/players` credentials are stored atomically with
    `flutter_secure_storage` (iOS Keychain / Android Keystore-backed storage),
    scoped by normalized server origin so development servers cannot share a
    mismatched id/token pair. Secure storage stays behind an injected interface
    for deterministic tests; the token is never rendered.
  - A typed REST client covers room list/create and matchmaking join/leave/status,
    strictly parses success bodies, preserves stable backend error codes, and
    uses an abortable long-poll so timeout, cancel, and screen disposal release
    queue membership.
  - The lobby supports one- or two-sequence games, shows queue depth and rooms,
    creates/selects rooms, finds an opponent, and emits a match-id/seat handoff
    for M3/M4. Direct selection is explicitly not presented as a seated join:
    the backend claims that seat on the M4 WebSocket connection.
  - Platform hardening: required iOS Keychain entitlement; Android backup
    disabled so encrypted credentials are not restored without their device key.
  - Gate: `flutter test` 41/41 ✅; `flutter analyze` clean ✅. This host has no
    Android SDK or full Xcode installation, so platform builds and the manual
    two-emulator matchmaking check remain documented in `mobile/README.md`.
- [x] **M3 — Board rendering** _(PR: m3-board-rendering)_
  - A typed Dart model validates the complete authoritative REST snapshot at the
    network boundary: 100 unique cells, correct wild corners/card presence,
    bounded and unique chips/seats, private-hand rules, status/winner
    consistency, and every card/rank/suit enum.
  - Lobby handoffs now carry the existing identity proof into an authenticated
    `GET /v1/matches/{id}` and navigate to a read-only match screen. Matchmade
    players receive their private view; direct rooms remain honest spectator
    views until M4 opens the seat-claiming WebSocket.
  - The responsive square board renders a 10×10 grid, red/black card faces,
    gold free corners, distinct player chips, and a gold ring for chips locked
    into a completed sequence. Turn/winner/viewer/presence context plus loading,
    retry, and manual refresh states surround the board.
  - Gate: `flutter test` 50/50 ✅; `flutter analyze` clean ✅. The end-to-end
    health → lobby → authenticated board route is widget-tested; this host
    still lacks Android SDK/full Xcode, so the emulator visual check remains
    documented in `mobile/README.md`.
- [ ] M4 WebSocket wiring
- [ ] M5 full game UI
- [ ] M6 reconnection UX
