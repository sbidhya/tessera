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
- [x] **B1 — Pure game engine (no I/O)** _(PR: b1-pure-engine)_
  - `internal/engine`: `Card`/`Deck` (double 104-card deck, seeded `*rand.Rand` shuffle), `Board` (immutable 10×10, 48 non-jack cards ×2 + 4 corners wild, deterministic layout + `PositionsFor`), `GameState`/`Player`/`Move` + pure `ValidateMove`/`ApplyMove` (clone-and-apply, no I/O).
  - Rules: deal (configurable `HandSize`, default 7), play-card→place-chip, two-eyed jacks (wild placement), one-eyed jacks (remove opponent non-locked chip), dead-card discard (both cells occupied, once per turn), draw replacement, turn advance, `Locked` grid for sequence chips.
  - Sequence detection: 5-in-a-row H/V/Diag with corners wild; greedy disjoint packing so overlapping 6-runs count as one, corners reusable; `FindSequences`/`AllSequences`/`LockedGrid`.
  - Win: `SequencesToWin` configurable (default 2, 1 for fast test games); winner detection via packed counts.
  - Gate: table-driven tests — horizontal/vertical/diag/corner sequences, overlapping packing, both jack types, illegal/out-of-turn/win conditions ✅; `go vet` ✅; `go test -race ./...` clean ✅.
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
