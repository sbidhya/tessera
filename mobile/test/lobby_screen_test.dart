import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/identity_store.dart';
import 'package:tessera/lobby_screen.dart';
import 'package:tessera/tessera_api.dart';

/// M2 widget tests: the lobby screen against a mock HTTP client.
///
/// The screen fires a `/healthz` check and an identity load on launch; each
/// test pumps it with canned responses and asserts the cards it renders.
/// Async flows held open by a [Completer] use `pump()`, not `pumpAndSettle()`
/// (the waiting spinner never settles); completed flows use `pumpAndSettle()`.

const _healthBody = '{"status":"ok","uptime":"1.23s"}';
const _playerBody = '{"player_id":"p_9f2c1234","token":"p_9f2c1234.sig"}';

/// Routing mock: healthy `/healthz` by default, per-test overrides for the
/// M2 routes. Every request is logged for assertions.
class LobbyMock {
  final List<http.BaseRequest> requests = [];
  late final MockClient client;

  Future<http.Response> Function(http.BaseRequest request)? onPlayers;
  Future<http.Response> Function(http.BaseRequest request)? onJoin;
  Future<http.Response> Function(http.BaseRequest request)? onLeave;
  Future<http.Response> Function(http.BaseRequest request)? onCreateMatch;
  Future<http.Response> Function(http.BaseRequest request)? onListMatches;

  LobbyMock() {
    client = MockClient((request) async {
      requests.add(request);
      switch (request.url.path) {
        case '/healthz':
          return http.Response(_healthBody, 200);
        case '/v1/players':
          return onPlayers != null
              ? onPlayers!(request)
              : http.Response(_playerBody, 201);
        case '/v1/matchmaking/join':
          return onJoin != null
              ? onJoin!(request)
              : http.Response(
                  '{"match_id":"r_ab12cd34","seat":1,"player_id":"p_9f2c1234"}',
                  200,
                );
        case '/v1/matchmaking/leave':
          return onLeave != null
              ? onLeave!(request)
              : http.Response('{"cancelled":true}', 200);
        case '/v1/matches':
          if (request.method == 'POST') {
            return onCreateMatch != null
                ? onCreateMatch!(request)
                : http.Response(
                    '{"match":{"id":"r_newmatch","seq":1,"status":"waiting",'
                    '"players":0,"present":0,"capacity":2,"sequences_to_win":2}}',
                    201,
                  );
          }
          return onListMatches != null
              ? onListMatches!(request)
              : http.Response('{"matches":[]}', 200);
        default:
          return http.Response('not found', 404);
      }
    });
  }

  bool requested(String path) => requests.any((r) => r.url.path == path);
}

Future<void> pumpLobby(
  WidgetTester tester,
  LobbyMock mock, {
  IdentityStore? store,
}) async {
  // The lobby is a scrolling column of cards; a tall viewport keeps every
  // button hittable without scrolling (the 800×600 default leaves the
  // matches card below the fold).
  tester.view.physicalSize = const Size(800, 1600);
  tester.view.devicePixelRatio = 1.0;
  addTearDown(tester.view.resetPhysicalSize);

  await tester.pumpWidget(
    MaterialApp(
      home: LobbyScreen(
        httpClient: mock.client,
        identityStore: store ?? InMemoryIdentityStore(),
      ),
    ),
  );
  await tester.pumpAndSettle();
}

void main() {
  group('LobbyScreen', () {
    testWidgets('shows server status and identity prompt on launch', (
      tester,
    ) async {
      final mock = LobbyMock();
      await pumpLobby(tester, mock);

      expect(find.textContaining('Server reachable'), findsOneWidget);
      expect(find.text('No player identity yet'), findsOneWidget);
      // Lobby actions need an identity.
      final findButton = tester.widget<FilledButton>(
        find.byKey(const Key('findMatchButton')),
      );
      expect(findButton.onPressed, isNull);
      final createButton = tester.widget<OutlinedButton>(
        find.byKey(const Key('createMatchButton')),
      );
      expect(createButton.onPressed, isNull);
    });

    testWidgets('create identity shows the player and enables the lobby', (
      tester,
    ) async {
      final mock = LobbyMock();
      final store = InMemoryIdentityStore();
      await pumpLobby(tester, mock, store: store);

      await tester.tap(find.byKey(const Key('createIdentityButton')));
      await tester.pumpAndSettle();

      expect(mock.requested('/v1/players'), isTrue);
      expect(find.textContaining('p_9f2c'), findsOneWidget);
      // Persisted: a fresh screen with the same store skips the prompt.
      expect(await store.load(), isNotNull);
      final findButton = tester.widget<FilledButton>(
        find.byKey(const Key('findMatchButton')),
      );
      expect(findButton.onPressed, isNotNull);
    });

    testWidgets('restores a saved identity on launch', (tester) async {
      final mock = LobbyMock();
      final store = InMemoryIdentityStore();
      await store.save(
        const PlayerIdentity(playerId: 'p_saved', token: 'p_saved.sig'),
      );
      await pumpLobby(tester, mock, store: store);

      expect(find.textContaining('p_saved'), findsOneWidget);
      expect(mock.requested('/v1/players'), isFalse);
    });

    testWidgets('auth_disabled explains legacy-mode servers', (tester) async {
      final mock = LobbyMock();
      mock.onPlayers = (request) async => http.Response(
        '{"error":{"code":"auth_disabled","message":"no identity layer"}}',
        503,
      );
      await pumpLobby(tester, mock);

      await tester.tap(find.byKey(const Key('createIdentityButton')));
      await tester.pumpAndSettle();

      expect(find.textContaining('without the identity layer'), findsOneWidget);
    });

    testWidgets('find match pairs and shows the active match card', (
      tester,
    ) async {
      final mock = LobbyMock();
      await pumpLobby(tester, mock);

      await tester.tap(find.byKey(const Key('createIdentityButton')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('findMatchButton')));
      await tester.pumpAndSettle();

      expect(mock.requested('/v1/matchmaking/join'), isTrue);
      expect(find.byKey(const Key('activeMatchCard')), findsOneWidget);
      expect(find.textContaining('r_ab12cd34'), findsOneWidget);
      expect(find.textContaining('seat 1'), findsOneWidget);
    });

    testWidgets('cancel search leaves the queue and ignores the stale poll', (
      tester,
    ) async {
      final mock = LobbyMock();
      final joinGate = Completer<http.Response>();
      mock.onJoin = (request) => joinGate.future;
      await pumpLobby(tester, mock);

      await tester.tap(find.byKey(const Key('createIdentityButton')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('findMatchButton')));
      await tester.pump(); // searching spinner is up; never settles
      expect(find.byKey(const Key('searchingLabel')), findsOneWidget);

      await tester.tap(find.byKey(const Key('cancelSearchButton')));
      await tester.pump();
      await tester.pump();
      expect(mock.requested('/v1/matchmaking/leave'), isTrue);
      expect(find.text('Left the queue.'), findsOneWidget);

      // The stale long-poll ends with 204 after the cancel: no match card.
      joinGate.complete(http.Response('', 204));
      await tester.pumpAndSettle();
      expect(find.byKey(const Key('activeMatchCard')), findsNothing);
    });

    testWidgets('create match shows the active match card', (tester) async {
      final mock = LobbyMock();
      await pumpLobby(tester, mock);

      await tester.tap(find.byKey(const Key('createIdentityButton')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('createMatchButton')));
      await tester.pumpAndSettle();

      expect(mock.requested('/v1/matches'), isTrue);
      expect(find.byKey(const Key('activeMatchCard')), findsOneWidget);
      expect(find.textContaining('r_newmatch'), findsOneWidget);
    });

    testWidgets('refresh renders the public match list', (tester) async {
      final mock = LobbyMock();
      mock.onListMatches = (request) async => http.Response(
        '{"matches":[{"id":"r_listedmatch","seq":2,"status":"waiting",'
        '"players":1,"present":1,"capacity":2,"sequences_to_win":1}]}',
        200,
      );
      await pumpLobby(tester, mock);

      await tester.tap(find.byKey(const Key('refreshMatchesButton')));
      await tester.pumpAndSettle();

      expect(find.textContaining('r_listed'), findsOneWidget);
      expect(find.textContaining('1/2 players'), findsOneWidget);
    });

    testWidgets('forget identity returns to the prompt', (tester) async {
      final mock = LobbyMock();
      final store = InMemoryIdentityStore();
      await store.save(
        const PlayerIdentity(playerId: 'p_saved', token: 'p_saved.sig'),
      );
      await pumpLobby(tester, mock, store: store);

      await tester.tap(find.byKey(const Key('forgetIdentityButton')));
      await tester.pumpAndSettle();

      expect(find.text('No player identity yet'), findsOneWidget);
      expect(await store.load(), isNull);
    });

    testWidgets('shortId truncates long ids only', (tester) async {
      expect(shortId('p_9f2c1234'), 'p_9f2c12…');
      expect(shortId('short'), 'short');
    });
  });
}
