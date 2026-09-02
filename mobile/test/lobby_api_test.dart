import 'dart:async';
import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/lobby_api.dart';
import 'package:tessera/server_url.dart';

import 'support/match_fixture.dart';

const credentials = PlayerCredentials(playerId: 'p_alice', token: 'proof');

Map<String, Object> matchJson(String id) => {
  'id': id,
  'seq': 1,
  'status': 'waiting',
  'players': 0,
  'present': 0,
  'capacity': 2,
  'sequences_to_win': 2,
};

void main() {
  group('server URL', () {
    test('normalizes local input to an origin', () {
      expect(
        normalizeServerUrl(' localhost:8080/path '),
        'http://localhost:8080',
      );
      expect(
        serverEndpoint('https://example.com/base?q=1', 'v1/players').toString(),
        'https://example.com/v1/players',
      );
    });

    test('rejects empty and non-HTTP URLs', () {
      expect(() => normalizeServerUrl(''), throwsFormatException);
      expect(
        () => normalizeServerUrl('file:///tmp/tessera'),
        throwsFormatException,
      );
    });
  });

  test('creates and parses a server-issued identity', () async {
    final api = TesseraApi(
      baseUrl: 'localhost:8080',
      client: MockClient((request) async {
        expect(request.method, 'POST');
        expect(request.url.path, '/v1/players');
        expect(request.body, isEmpty);
        return http.Response('{"player_id":"p_alice","token":"proof"}', 201);
      }),
    );

    final result = await api.createPlayer();
    expect(result.playerId, 'p_alice');
    expect(result.token, 'proof');
  });

  test('lists and creates typed matches with authenticated options', () async {
    final requested = <String>[];
    final api = TesseraApi(
      baseUrl: 'http://server.test/',
      client: MockClient((request) async {
        requested.add('${request.method} ${request.url.path}');
        if (request.method == 'GET') {
          return http.Response(
            jsonEncode({
              'matches': [matchJson('r_one')],
            }),
            200,
          );
        }
        final body = jsonDecode(request.body) as Map<String, dynamic>;
        expect(body, {
          'player_id': 'p_alice',
          'token': 'proof',
          'sequences_to_win': 1,
        });
        return http.Response(jsonEncode({'match': matchJson('r_two')}), 201);
      }),
    );

    final matches = await api.listMatches();
    final created = await api.createMatch(credentials, sequencesToWin: 1);
    expect(matches.single.id, 'r_one');
    expect(created.id, 'r_two');
    expect(requested, ['GET /v1/matches', 'POST /v1/matches']);
  });

  test('loads a typed private match state with identity proof', () async {
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient((request) async {
        expect(request.method, 'GET');
        expect(request.url.path, '/v1/matches/r_board');
        expect(request.url.queryParameters, credentials.toJson());
        return matchSnapshotResponse();
      }),
    );

    final snapshot = await api.getMatchState('r_board', credentials);
    expect(snapshot.seq, 8);
    expect(snapshot.state.board, hasLength(100));
    expect(snapshot.state.viewer, 0);
  });

  test(
    'matchmaking join, status, and leave follow the B6 wire contract',
    () async {
      final api = TesseraApi(
        baseUrl: 'http://server.test',
        client: MockClient((request) async {
          switch ('${request.method} ${request.url.path}') {
            case 'GET /v1/matchmaking/status':
              return http.Response('{"waiting":3}', 200);
            case 'POST /v1/matchmaking/join':
              expect(jsonDecode(request.body), {
                'player_id': 'p_alice',
                'token': 'proof',
                'sequences_to_win': 2,
              });
              return http.Response(
                '{"match_id":"r_pair","seat":1,"player_id":"p_alice"}',
                200,
              );
            case 'POST /v1/matchmaking/leave':
              expect(jsonDecode(request.body), credentials.toJson());
              return http.Response('{"cancelled":true}', 200);
            default:
              return http.Response('not found', 404);
          }
        }),
      );

      expect(await api.matchmakingStatus(), 3);
      final paired = await api.joinMatchmaking(credentials, sequencesToWin: 2);
      expect(paired?.matchId, 'r_pair');
      expect(paired?.seat, 1);
      expect(await api.leaveMatchmaking(credentials), isTrue);
    },
  );

  test('204 matchmaking response is a normal cancelled search', () async {
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient((request) async => http.Response('', 204)),
    );
    expect(await api.joinMatchmaking(credentials, sequencesToWin: 2), isNull);
  });

  test('an abort trigger cancels the long-poll with a stable code', () async {
    final abort = Completer<void>();
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient.streaming((request, bodyStream) async {
        expect(request, isA<http.AbortableRequest>());
        final abortable = request as http.AbortableRequest;
        await abortable.abortTrigger;
        throw http.RequestAbortedException(request.url);
      }),
    );

    final result = api.joinMatchmaking(
      credentials,
      sequencesToWin: 2,
      abortTrigger: abort.future,
    );
    abort.complete();
    await expectLater(
      result,
      throwsA(
        isA<TesseraApiException>().having(
          (error) => error.code,
          'code',
          'request_aborted',
        ),
      ),
    );
  });

  test('rejects matchmaking details issued for another player', () async {
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient(
        (request) async => http.Response(
          '{"match_id":"r_pair","seat":0,"player_id":"p_bob"}',
          200,
        ),
      ),
    );
    await expectLater(
      api.joinMatchmaking(credentials, sequencesToWin: 2),
      throwsA(
        isA<TesseraApiException>().having(
          (error) => error.message,
          'message',
          contains('another player'),
        ),
      ),
    );
  });

  test('preserves backend error code, message, and status', () async {
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient(
        (request) async => http.Response(
          '{"error":{"code":"invalid_token","message":"bad proof"}}',
          401,
        ),
      ),
    );

    await expectLater(
      api.createMatch(credentials, sequencesToWin: 2),
      throwsA(
        isA<TesseraApiException>()
            .having((error) => error.code, 'code', 'invalid_token')
            .having((error) => error.statusCode, 'status', 401)
            .having((error) => error.message, 'message', 'bad proof'),
      ),
    );
  });

  test('maps malformed success bodies and request timeouts', () async {
    final malformed = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient(
        (request) async => http.Response('{"matches":42}', 200),
      ),
    );
    await expectLater(
      malformed.listMatches(),
      throwsA(isA<TesseraApiException>()),
    );

    final timeout = TesseraApi(
      baseUrl: 'http://server.test',
      requestTimeout: const Duration(milliseconds: 10),
      client: MockClient((request) async {
        await Future<void>.delayed(const Duration(seconds: 1));
        return http.Response('{}', 200);
      }),
    );
    await expectLater(
      timeout.matchmakingStatus(),
      throwsA(
        isA<TesseraApiException>().having(
          (error) => error.code,
          'code',
          'timeout',
        ),
      ),
    );
  });
}
