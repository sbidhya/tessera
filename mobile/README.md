# Tessera mobile (Flutter) — M2: identity + lobby

The Flutter app now checks backend health, keeps an anonymous player identity,
and exposes the REST lobby. M2 can:

- issue `POST /v1/players` once and retain the id/token pair in iOS Keychain or
  Android Keystore-backed secure storage;
- keep independent credentials for different server origins;
- list and create rooms with one- or two-sequence rules;
- show matchmaking queue depth, wait for a compatible opponent, and leave the
  queue on cancel; and
- hand a selected/created/paired match id to the next mobile layer.

The token is never rendered. A player id is not a password, so it is safe to
show for debugging. Direct room selection does not claim a seat yet: the B6
server intentionally joins seats on WebSocket connect, which arrives in M4.
M3 will consume the match handoff first to render authoritative REST state.

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

`localhost` on a phone means the phone itself:

- Android emulator: use `http://10.0.2.2:8080` (pre-filled).
- iOS simulator: use `http://localhost:8080` (pre-filled).

For the manual matchmaking gate, run the backend and launch the app on two
emulators. Pick the same sequence target on both, tap **Find opponent**, and
confirm both receive the same match id with different seats. Also cancel one
search and confirm the displayed queue count returns to zero after refresh.

## Tests

```sh
cd mobile
flutter test
flutter analyze
```

The 41 tests use injected HTTP and credential-store fakes, so they need neither
a backend nor platform keychain access. They cover URL handling, identity reuse
and server scoping, every M2 REST shape, backend error decoding, room creation
and selection, successful matchmaking, queue cancellation, and the M1 health
flow. `flutter analyze` should report `No issues found!`.

## Security/platform notes

- Credentials are written as one JSON value so a crash cannot leave a player id
  paired with the wrong token.
- Android backup is disabled because restoring encrypted preferences without
  the original device key can make them unreadable.
- The iOS runner includes Keychain Sharing entitlements required by
  `flutter_secure_storage`.
- Losing or deliberately clearing secure app storage loses this anonymous
  identity. There is no account-recovery service by design.

## Layout

- `lib/server_health.dart`, `lib/health_screen.dart` — M1 connectivity probe.
- `lib/server_url.dart` — shared URL normalization for all backend routes.
- `lib/lobby_api.dart` — typed M2 REST client and wire models.
- `lib/identity_store.dart` — secure per-server credential persistence.
- `lib/lobby_screen.dart` — room list/create and matchmaking UI.
- `test/` — unit and widget tests with injected fakes.
