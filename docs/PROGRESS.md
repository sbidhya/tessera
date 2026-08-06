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
- [ ] **B2 — In-memory room manager** ← next
- [ ] **B3 — Transport (HTTP + WebSocket)**
- [ ] **B4 — Durability: WAL + replay**
- [ ] **B5 — Cold tier: SQLite + write-behind**
- [ ] **B6 — Matchmaking, presence, light auth**

## Mobile (Flutter)

- [ ] M1 skeleton → `/healthz` status
- [ ] M2 identity + lobby
- [ ] M3 board rendering
- [ ] M4 WebSocket wiring
- [ ] M5 full game UI
- [ ] M6 reconnection UX
