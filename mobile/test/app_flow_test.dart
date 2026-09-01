import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/identity_store.dart';
import 'package:tessera/lobby_api.dart';
import 'package:tessera/main.dart';

import 'support/match_fixture.dart';

class _MemoryCredentials implements CredentialStore {
  @override
  Future<PlayerCredentials?> read(String serverUrl) async =>
      const PlayerCredentials(playerId: 'p_alice', token: 'proof');

  @override
  Future<void> write(String serverUrl, PlayerCredentials credentials) async {}

  @override
  Future<void> delete(String serverUrl) async {}
}

void main() {
  testWidgets('health to lobby to static board is one complete app flow', (
    tester,
  ) async {
    final client = MockClient((request) async {
      switch ('${request.method} ${request.url.path}') {
        case 'GET /healthz':
          return http.Response('{"status":"ok","uptime":"1s"}', 200);
        case 'GET /v1/matches':
          return http.Response(
            jsonEncode({
              'matches': [
                {
                  'id': 'r_board',
                  'seq': 8,
                  'status': 'playing',
                  'players': 2,
                  'present': 1,
                  'capacity': 2,
                  'sequences_to_win': 2,
                },
              ],
            }),
            200,
          );
        case 'GET /v1/matchmaking/status':
          return http.Response('{"waiting":0}', 200);
        case 'GET /v1/matches/r_board':
          expect(request.url.queryParameters, {
            'player_id': 'p_alice',
            'token': 'proof',
          });
          return matchSnapshotResponse();
        default:
          return http.Response('not found', 404);
      }
    });

    await tester.pumpWidget(
      TesseraApp(httpClient: client, credentialStore: _MemoryCredentials()),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const Key('openLobbyButton')));
    await tester.pumpAndSettle();
    await tester.ensureVisible(find.byKey(const ValueKey('open-r_board')));
    await tester.tap(find.byKey(const ValueKey('open-r_board')));
    await tester.pumpAndSettle();

    expect(find.text('Match r_board'), findsOneWidget);
    expect(find.byKey(const Key('sequenceBoard')), findsOneWidget);
    expect(find.byKey(const Key('chip-0-1')), findsOneWidget);
  });
}
