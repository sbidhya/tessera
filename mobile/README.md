# Tessera mobile (Flutter) — M3: static board rendering

The Flutter app now checks backend health, keeps an anonymous player identity,
exposes the REST lobby, and opens an authoritative read-only match view. M3 can:

- fetch the viewer-specific `GET /v1/matches/{id}` snapshot with the stored
  identity proof;
- strictly decode the full wire state, including board geometry, cards, chips,
  sequences, seats, presence, turn, and winner;
- render all 100 cells in a responsive square board with suit-colored card
  faces, four free corners, player-colored chips, and a highlighted ring for
  chips locked into a completed sequence; and
- show viewer/turn/player status plus explicit loading, retry, and manual
  refresh states.

The token is never rendered. A player id is not a password, so it is safe to
show for debugging. Direct room selection does not claim a seat yet: the B6
server intentionally joins seats on WebSocket connect, which arrives in M4, so
directly created/selected rooms render as spectator views for now. Matchmaking
players already own seats and receive their private viewer state. M3 is static
by design; use the refresh button to request a newer snapshot.

## Prerequisites

- [Flutter SDK](https://docs.flutter.dev/get-started/install) 3.47+ (Dart 3.13+).
  This session used `/tmp/sdks/flutter`; `/tmp` is cleared on reboot, so install
  Flutter somewhere permanent for regular development.
- The backend running from the repo root:

  ```sh
  cd backend
  make run
  ```

## Run the app

```sh
cd mobile
flutter pub get
flutter run
```

The first screen checks `/healthz`. After it succeeds, tap **Open lobby**. The
lobby creates an identity on first use and securely reuses it on later launches.
Create, select, or matchmake into a room to open its current board.

`localhost` on a phone means the phone itself:

- Android emulator: use `http://10.0.2.2:8080` (pre-filled).
- iOS simulator: use `http://localhost:8080` (pre-filled).

For the M3 manual gate, run the backend and launch the app on an emulator. Open
a created room and confirm a 10×10 spectator board appears, then matchmake from
two emulators and confirm each opens the same board as its own player seat.
Moves remain intentionally unavailable until M4 adds WebSockets.

## Tests

```sh
cd mobile
flutter test
flutter analyze
```

The 50 tests use injected HTTP and credential-store fakes, so they need neither
a backend nor platform keychain access. M3 coverage includes strict state
decoding and rejection, authenticated state requests, all 100 rendered cells,
corners, regular and completed-sequence chips, retry/refresh behavior, and the
complete health → lobby → board route. The M1/M2 tests remain in the suite.
`flutter analyze` should report `No issues found!`.

## Security/platform notes

- Credentials are written as one JSON value so a crash cannot leave a player id
  paired with the wrong token.
- The B6 REST contract sends private-view tokens as query parameters. Use HTTPS
  anywhere beyond local emulator development and redact query strings from
  production access logs.
- Android backup is disabled because restoring encrypted preferences without
  the original device key can make them unreadable.
- The iOS runner includes Keychain Sharing entitlements required by
  `flutter_secure_storage`.
- Losing or deliberately clearing secure app storage loses this anonymous
  identity. There is no account-recovery service by design.

## Layout

- `lib/server_health.dart`, `lib/health_screen.dart` — M1 connectivity probe.
- `lib/server_url.dart` — shared URL normalization for all backend routes.
- `lib/lobby_api.dart` — typed M2/M3 REST client and lobby wire models.
- `lib/identity_store.dart` — secure per-server credential persistence.
- `lib/lobby_screen.dart` — room list/create and matchmaking UI.
- `lib/game_state.dart` — validated M3 authoritative-state wire model.
- `lib/match_screen.dart` — static match status and 10×10 board renderer.
- `test/` — unit and widget tests with injected fakes.
