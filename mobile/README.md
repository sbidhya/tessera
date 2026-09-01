# Tessera mobile (Flutter) — M2: identity + lobby

Small Flutter app (iOS + Android) for the Sequence game backend. **M1** proved
the app can reach the backend (`GET /healthz`); **M2** adds the lobby: mint an
anonymous player identity, find a partner through the matchmaking queue,
create a match directly, or browse the public match list. Later blocks add
board rendering (M3), WebSocket play (M4–M5), and reconnection UX (M6).

## Prerequisites

- [Flutter SDK](https://docs.flutter.dev/get-started/install) 3.47+ (Dart 3.13+).
  This session used a local SDK; point your shell at it, e.g.:
  `export PATH=/tmp/sdks/flutter/bin:$PATH` (re-install to a permanent path —
  `/tmp` is cleared on reboot).
- The backend running (from the repo root):
  `cd backend && make run` — serves `GET /healthz` on `:8080` by default
  (`TESSERA_ADDR` overrides).

## Run the app

```sh
cd mobile
flutter pub get
flutter run
```

The screen checks `/healthz` on launch and shows `status` / `uptime`, or the
failure reason. The server URL is editable in the app.

**Emulator networking note:** `localhost` on a phone means the phone itself.
- Android emulator → use `http://10.0.2.2:8080` (pre-filled on Android).
- iOS simulator → `http://localhost:8080` works (pre-filled on iOS).

## Using the lobby (M2)

1. Check the server row is green (same `/healthz` probe as M1).
2. **Create player identity** — mints an anonymous `p_…` id + token via
   `POST /v1/players`. The lobby buttons stay disabled until you have one.
3. Pick **1 (quick)** or **2 (standard)** sequences to win, then either:
   - **Find match (matchmaking)** — waits in the queue (~30s budget, then
     tap again to keep waiting); **Cancel search** withdraws via
     `POST /v1/matchmaking/leave`. When paired, the match card shows the
     match id and your seat.
   - **Create match directly** — shows the new match id to share.
   - **Refresh** the open-matches list (public, no identity needed).

The identity lives in an in-memory `IdentityStore` for now
(`lib/identity_store.dart`); per `docs/match.md` it should move to the
platform keychain/keystore (`flutter_secure_storage`) before this app is
anything but a learning client — the `IdentityStore` seam makes that a
one-class swap. The board arrives in M3/M4; the match card tells you the id
and seat to play from.

## Tests

```sh
cd mobile
flutter test          # 48 tests: /healthz client + screen (M1), identity/
                      # lobby REST client + lobby screen (M2) — mock HTTP
flutter analyze       # clean — no issues
```

The tests use a mock HTTP client, so no backend is needed. The live-server
gate is manual: run the backend, run the app, and confirm the lobby reaches
it (green server row, identity mints, matchmaking pairs two emulators).
Turning the backend off should flip the row to "Server unreachable".
A throwaway live smoke test (real `TesseraApi` against a local backend —
two clients paired into one match, seats {0, 1}) passed during development;
it is not committed since it needs a live server.

## Layout

- `lib/main.dart` — entry point; owns the single `http.Client` and the
  `IdentityStore`, shows the lobby.
- `lib/server_health.dart` — `GET /healthz` client + `ServerHealth` model
  (also reused by the lobby as the connectivity probe) and the per-platform
  `defaultBaseUrl`.
- `lib/health_screen.dart` — the M1 status screen (still tested; no longer
  the app's home).
- `lib/tessera_api.dart` — M2 REST client: `POST /v1/players`,
  `POST|GET /v1/matches`, `POST /v1/matchmaking/join|leave`,
  `GET /v1/matchmaking/status`. One `TesseraApiException` type carrying the
  server's stable error `code`.
- `lib/identity_store.dart` — `IdentityStore` seam + in-memory
  implementation.
- `lib/lobby_screen.dart` — the M2 lobby UI.
- `test/` — unit tests for the clients, widget tests for the screens.
