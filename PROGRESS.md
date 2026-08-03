# Tessera — Progress

This file tracks completed building blocks so the next task can resume from the last checkpoint.

## Completed

### B0 — Skeleton & tooling ✅
- **What it does**: Go module, repo layout, Makefile, `/healthz` HTTP endpoint, structured logging (`log/slog`), config with seeded RNG (`TESSERA_*` env vars + `Config.RNG()`).
- **Key design choices**:
  - `log/slog` (stdlib) for structured JSON logging — zero dependencies, aggregator-friendly.
  - Single `Config.Seed` drives all randomness via `Config.RNG()` so tests are deterministic.
  - `/healthz` is cheap, stateless, no DB/room dependency; supports GET + HEAD, rejects other methods with 405.
  - Strict layering: `transport` owns HTTP, `config`/`logger` are pure infra; future `engine` will not import them.
- **Gate**: `go test ./...` and `go test -race ./...` pass; `curl localhost:8080/healthz` → `{"status":"ok"}` 200.
- **How to verify**:
  ```bash
  make vet && make race
  make run # in one terminal
  curl -i http://localhost:8080/healthz  # in another → 200 + {"status":"ok"}
  # or: go test ./... -v; go test -race ./...
  ```
- **Files**: `backend/go.mod`, `backend/cmd/tessera/main.go`, `backend/internal/config/*`, `backend/internal/logger/*`, `backend/internal/transport/*`, `backend/Makefile`, `Makefile`
- **Completed**: 2026-08-03
- **PR**: #TBD

## Next up

### B1 — Pure game engine (no I/O) ⏳ (next)
- Implement `Card`, `Deck`, `Board` (cell↔card map + corners), `Player`, `Chip`, `GameState`; rules for deal, play-card→place-chip, both jack types, dead-card swap, draw, sequence detection, win. All pure functions, seeded RNG.
- Gate: table-driven tests for every sequence direction incl. corners, jack behaviors, illegal/out-of-turn moves, win conditions; `go test -race`.

## Backlog (do not start until requested)
- B2 In-memory room manager (actor model, command channel)
- B3 Transport (REST + WebSocket + typed envelope)
- B4 WAL + replay
- B5 SQLite write-behind + checkpoint
- B6 Matchmaking, presence, light auth
- M1–M6 Mobile (Flutter)
