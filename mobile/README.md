# Tessera mobile

The Flutter client currently implements M1: it checks the backend's
`GET /healthz` endpoint at startup, displays the reported status and uptime,
and offers a retry action when the backend cannot be reached.

## Run locally

Start the backend from the repository root:

```sh
cd backend
go run ./cmd/tessera
```

In another terminal, launch the app:

```sh
cd mobile
flutter run
```

The default backend URL is `http://10.0.2.2:8080` on an Android emulator and
`http://127.0.0.1:8080` on an iOS simulator. For a physical device or another
environment, provide a reachable URL at build time:

```sh
flutter run --dart-define=TESSERA_BACKEND_URL=http://192.168.1.10:8080
```

Run the M1 checks with:

```sh
flutter analyze
flutter test
```

A green run reports `No issues found!` and `All tests passed!`.
