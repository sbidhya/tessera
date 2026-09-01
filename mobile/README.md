# Tessera mobile (Flutter) — M1: server status

Small Flutter app (iOS + Android) for the Sequence game backend. **M1** proves
the app can reach the backend: it fetches `GET /healthz` and shows the server
status. Later blocks add identity/lobby (M2), board rendering (M3), WebSocket
play (M4–M5), and reconnection UX (M6).

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

## Tests

```sh
cd mobile
flutter test          # 17 tests: /healthz client + status screen (mock HTTP)
flutter analyze       # clean — no issues
```

The tests use a mock HTTP client, so no backend is needed. The live-server
gate is manual: run the backend, run the app, and confirm it shows
"Server reachable" with the backend's uptime. Turning the backend off should
flip the card to "Server unreachable".

## Layout

- `lib/main.dart` — entry point; owns the single `http.Client`.
- `lib/server_health.dart` — `GET /healthz` client + `ServerHealth` model
  (pure Dart; also reused by later blocks as a connectivity probe).
- `lib/health_screen.dart` — the M1 status screen.
- `test/` — unit tests for the client, widget tests for the screen.
