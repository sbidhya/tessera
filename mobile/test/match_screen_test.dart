import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/lobby_api.dart';
import 'package:tessera/match_screen.dart';

import 'support/match_fixture.dart';

const credentials = PlayerCredentials(playerId: 'p_alice', token: 'proof');

Future<void> pumpMatch(WidgetTester tester, http.Client client) async {
  await tester.pumpWidget(
    MaterialApp(
      home: MatchScreen(
        httpClient: client,
        baseUrl: 'http://server.test',
        matchId: 'r_board',
        credentials: credentials,
      ),
    ),
  );
  await tester.pumpAndSettle();
}

void main() {
  testWidgets('renders all 100 cards, corners, chips, and viewer status', (
    tester,
  ) async {
    final semantics = tester.ensureSemantics();
    await pumpMatch(
      tester,
      MockClient((request) async {
        expect(request.method, 'GET');
        expect(request.url.path, '/v1/matches/r_board');
        expect(request.url.queryParameters, {
          'player_id': 'p_alice',
          'token': 'proof',
        });
        return matchSnapshotResponse();
      }),
    );

    expect(find.byType(SequenceBoardCell), findsNWidgets(100));
    expect(find.byKey(const Key('corner-0-0')), findsOneWidget);
    expect(find.byKey(const Key('card-0-1')), findsOneWidget);
    expect(find.byKey(const Key('chip-0-1')), findsOneWidget);
    expect(find.byKey(const Key('sequence-chip-5-5')), findsOneWidget);
    expect(
      find.bySemanticsLabel('Row 1, column 2, A of hearts, Player 1 chip'),
      findsOneWidget,
    );
    expect(find.text('Your turn'), findsOneWidget);
    expect(find.textContaining('You are Player 1'), findsOneWidget);
    expect(find.text('#8'), findsOneWidget);

    await tester.drag(find.byType(ListView), const Offset(0, -700));
    await tester.pumpAndSettle();
    expect(find.textContaining('Player 2 · away'), findsOneWidget);
    semantics.dispose();
  });

  testWidgets('shows a retryable load failure and then recovers', (
    tester,
  ) async {
    var calls = 0;
    await pumpMatch(
      tester,
      MockClient((request) async {
        calls++;
        if (calls == 1) {
          return http.Response(
            '{"error":{"code":"match_not_found","message":"gone"}}',
            404,
          );
        }
        return matchSnapshotResponse();
      }),
    );

    expect(find.text('gone'), findsOneWidget);
    expect(find.byKey(const Key('retryMatchButton')), findsOneWidget);

    await tester.tap(find.byKey(const Key('retryMatchButton')));
    await tester.pumpAndSettle();
    expect(calls, 2);
    expect(find.byKey(const Key('sequenceBoard')), findsOneWidget);
  });

  testWidgets('manual refresh replaces the static snapshot', (tester) async {
    var calls = 0;
    await pumpMatch(
      tester,
      MockClient((request) async {
        calls++;
        return matchSnapshotResponse(seq: calls == 1 ? 8 : 9);
      }),
    );
    expect(find.text('#8'), findsOneWidget);

    await tester.tap(find.byKey(const Key('refreshMatchButton')));
    await tester.pumpAndSettle();

    expect(calls, 2);
    expect(find.text('#9'), findsOneWidget);
  });
}
