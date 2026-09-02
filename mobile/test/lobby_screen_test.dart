import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/identity_store.dart';
import 'package:tessera/lobby_api.dart';
import 'package:tessera/lobby_screen.dart';

class MemoryCredentialStore implements CredentialStore {
  PlayerCredentials? value = const PlayerCredentials(
    playerId: 'p_saved',
    token: 'saved-proof',
  );

  @override
  Future<PlayerCredentials?> read(String serverUrl) async => value;

  @override
  Future<void> write(String serverUrl, PlayerCredentials credentials) async {
    value = credentials;
  }

  @override
  Future<void> delete(String serverUrl) async => value = null;
}

Map<String, Object> summary(String id, {int sequences = 2}) => {
  'id': id,
  'seq': 1,
  'status': 'waiting',
  'players': 0,
  'present': 0,
  'capacity': 2,
  'sequences_to_win': sequences,
};

http.Response baseResponse(http.Request request) {
  if (request.url.path == '/v1/matches' && request.method == 'GET') {
    return http.Response(
      jsonEncode({
        'matches': [summary('r_open')],
      }),
      200,
    );
  }
  if (request.url.path == '/v1/matchmaking/status') {
    return http.Response('{"waiting":1}', 200);
  }
  return http.Response('not found', 404);
}

Future<void> pumpLobby(WidgetTester tester, http.Client client) async {
  await tester.pumpWidget(
    MaterialApp(
      home: LobbyScreen(
        httpClient: client,
        credentialStore: MemoryCredentialStore(),
        baseUrl: 'http://server.test',
      ),
    ),
  );
  await tester.pumpAndSettle();
}

void main() {
  testWidgets('loads saved identity, queue count, and available rooms', (
    tester,
  ) async {
    await pumpLobby(
      tester,
      MockClient((request) async => baseResponse(request)),
    );

    expect(find.text('Tessera — Lobby'), findsOneWidget);
    expect(find.textContaining('p_saved'), findsOneWidget);
    expect(find.text('1 player waiting'), findsOneWidget);
    expect(find.text('r_open'), findsOneWidget);

    await tester.ensureVisible(find.byKey(const ValueKey('open-r_open')));
    await tester.tap(find.byKey(const ValueKey('open-r_open')));
    await tester.pump();
    expect(find.text('Room selected'), findsOneWidget);
    expect(find.textContaining('Match r_open'), findsOneWidget);
  });

  testWidgets('creates a quick room with identity proof and reports handoff', (
    tester,
  ) async {
    Map<String, dynamic>? createBody;
    final client = MockClient((request) async {
      if (request.url.path == '/v1/matches' && request.method == 'POST') {
        createBody = jsonDecode(request.body) as Map<String, dynamic>;
        return http.Response(
          jsonEncode({'match': summary('r_created', sequences: 1)}),
          201,
        );
      }
      return baseResponse(request);
    });
    await pumpLobby(tester, client);

    await tester.tap(find.text('Quick · 1 sequence'));
    await tester.ensureVisible(find.byKey(const Key('createMatchButton')));
    await tester.tap(find.byKey(const Key('createMatchButton')));
    await tester.pumpAndSettle();

    expect(createBody, {
      'player_id': 'p_saved',
      'token': 'saved-proof',
      'sequences_to_win': 1,
    });
    expect(find.text('Room created'), findsOneWidget);
    expect(find.textContaining('Match r_created'), findsOneWidget);
  });

  testWidgets('matchmaking returns a paired match and seat', (tester) async {
    final ready = <ReadyMatch>[];
    final client = MockClient((request) async {
      if (request.url.path == '/v1/matchmaking/join') {
        return http.Response(
          '{"match_id":"r_pair","seat":1,"player_id":"p_saved"}',
          200,
        );
      }
      return baseResponse(request);
    });
    await tester.pumpWidget(
      MaterialApp(
        home: LobbyScreen(
          httpClient: client,
          credentialStore: MemoryCredentialStore(),
          baseUrl: 'http://server.test',
          onMatchReady: ready.add,
        ),
      ),
    );
    await tester.pumpAndSettle();

    await tester.ensureVisible(find.byKey(const Key('findOpponentButton')));
    await tester.tap(find.byKey(const Key('findOpponentButton')));
    await tester.pumpAndSettle();

    expect(find.text('Opponent found'), findsOneWidget);
    expect(find.textContaining('Seat 1'), findsOneWidget);
    expect(ready.single.matchId, 'r_pair');
    expect(ready.single.credentials.playerId, 'p_saved');
  });

  testWidgets('cancel search leaves the server queue', (tester) async {
    final joinResponse = Completer<http.Response>();
    var leaveCalls = 0;
    final client = MockClient((request) async {
      if (request.url.path == '/v1/matchmaking/join') {
        return joinResponse.future;
      }
      if (request.url.path == '/v1/matchmaking/leave') {
        leaveCalls++;
        if (!joinResponse.isCompleted) {
          joinResponse.complete(http.Response('', 204));
        }
        return http.Response('{"cancelled":true}', 200);
      }
      return baseResponse(request);
    });
    await pumpLobby(tester, client);

    await tester.ensureVisible(find.byKey(const Key('findOpponentButton')));
    await tester.tap(find.byKey(const Key('findOpponentButton')));
    await tester.pump();
    expect(find.text('Looking for an opponent…'), findsOneWidget);

    await tester.tap(find.byKey(const Key('cancelMatchmakingButton')));
    await tester.pumpAndSettle();
    expect(leaveCalls, 1);
    expect(find.byKey(const Key('findOpponentButton')), findsOneWidget);
  });

  testWidgets('pairing that wins a cancel race is still delivered', (
    tester,
  ) async {
    final joinResponse = Completer<http.Response>();
    final client = MockClient((request) async {
      if (request.url.path == '/v1/matchmaking/join') {
        return joinResponse.future;
      }
      if (request.url.path == '/v1/matchmaking/leave') {
        joinResponse.complete(
          http.Response(
            '{"match_id":"r_race","seat":0,"player_id":"p_saved"}',
            200,
          ),
        );
        return http.Response('{"cancelled":false}', 200);
      }
      return baseResponse(request);
    });
    await pumpLobby(tester, client);

    await tester.ensureVisible(find.byKey(const Key('findOpponentButton')));
    await tester.tap(find.byKey(const Key('findOpponentButton')));
    await tester.pump();
    await tester.tap(find.byKey(const Key('cancelMatchmakingButton')));
    await tester.pumpAndSettle();

    expect(find.text('Opponent found'), findsOneWidget);
    expect(find.textContaining('Match r_race'), findsOneWidget);
  });

  testWidgets('bootstrap failures show a retryable error', (tester) async {
    await pumpLobby(
      tester,
      MockClient(
        (request) async => http.Response(
          '{"error":{"code":"matchmaking_disabled","message":"not enabled"}}',
          503,
        ),
      ),
    );

    expect(find.text('not enabled'), findsOneWidget);
    expect(find.byKey(const Key('retryLobbyButton')), findsOneWidget);
  });
}
