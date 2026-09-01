import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/tessera_api.dart';

/// M2 unit tests: the identity + lobby REST client against canned responses.
///
/// Like the M1 tests, these run on a mock HTTP client — no backend needed.
/// The wire contract (paths, status codes, JSON shapes, the `{"error": …}`
/// envelope) is pinned here; the live-server gate is manual.

const _identity = PlayerIdentity(playerId: 'p_abc', token: 'p_abc.sig');

http.Client routeClient(
  Future<http.Response> Function(http.BaseRequest request) handler,
) => MockClient(handler);

void main() {
  group('PlayerIdentity', () {
    test('round-trips through JSON', () {
      const id = PlayerIdentity(playerId: 'p_1', token: 'p_1.sig');
      final back = PlayerIdentity.fromJson(id.toJson());
      expect(back.playerId, 'p_1');
      expect(back.token, 'p_1.sig');
    });

    test('rejects malformed bodies', () {
      expect(
        () => PlayerIdentity.fromJson({'player_id': 'p_1'}),
        throwsA(isA<TesseraApiException>()),
      );
    });
  });

  group('createPlayer', () {
    test('parses the 201 identity body', () async {
      final client = routeClient((request) async {
        expect(request.url.path, '/v1/players');
        expect(request.method, 'POST');
        return http.Response(
          '{"player_id":"p_9f2c","token":"p_9f2c.hmac"}',
          201,
        );
      });
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      final identity = await api.createPlayer();
      expect(identity.playerId, 'p_9f2c');
      expect(identity.token, 'p_9f2c.hmac');
    });

    test(
      'surfaces auth_disabled when the server has no identity layer',
      () async {
        final client = routeClient(
          (request) async => http.Response(
            '{"error":{"code":"auth_disabled","message":"no identity layer"}}',
            503,
          ),
        );
        final api = TesseraApi(
          client: client,
          baseUrl: 'http://localhost:8080',
        );
        await expectLater(
          api.createPlayer(),
          throwsA(
            isA<TesseraApiException>()
                .having((e) => e.code, 'code', 'auth_disabled')
                .having((e) => e.statusCode, 'statusCode', 503),
          ),
        );
      },
    );
  });

  group('createMatch', () {
    test('sends exactly the documented fields and parses the match', () async {
      Map<String, dynamic>? seenBody;
      final client = routeClient((request) async {
        expect(request.url.path, '/v1/matches');
        final req = request as http.Request;
        seenBody = jsonDecode(req.body) as Map<String, dynamic>;
        return http.Response(
          '{"match":{"id":"r_1","seq":1,"status":"waiting","players":0,'
          '"present":0,"capacity":2,"sequences_to_win":1}}',
          201,
        );
      });
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      final match = await api.createMatch(
        identity: _identity,
        sequencesToWin: 1,
      );
      // The backend rejects unknown JSON fields, so the body must be exact.
      expect(seenBody, {
        'sequences_to_win': 1,
        'player_id': 'p_abc',
        'token': 'p_abc.sig',
      });
      expect(match.id, 'r_1');
      expect(match.sequencesToWin, 1);
      expect(match.capacity, 2);
    });

    test('maps 422 invalid_options to the server code', () async {
      final client = routeClient(
        (request) async => http.Response(
          '{"error":{"code":"invalid_options","message":"must be positive"}}',
          422,
        ),
      );
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      await expectLater(
        api.createMatch(identity: _identity, sequencesToWin: -1),
        throwsA(
          isA<TesseraApiException>().having(
            (e) => e.code,
            'code',
            'invalid_options',
          ),
        ),
      );
    });

    test('maps 401 invalid_token to the server code', () async {
      final client = routeClient(
        (request) async => http.Response(
          '{"error":{"code":"invalid_token","message":"bad token"}}',
          401,
        ),
      );
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      await expectLater(
        api.createMatch(identity: _identity),
        throwsA(
          isA<TesseraApiException>().having(
            (e) => e.code,
            'code',
            'invalid_token',
          ),
        ),
      );
    });
  });

  group('listMatches', () {
    test('parses the match list (and the empty list)', () async {
      for (final body in [
        '{"matches":[]}',
        '{"matches":[{"id":"r_1","seq":3,"status":"playing","players":2,'
            '"present":1,"capacity":2,"sequences_to_win":2}]}',
      ]) {
        final client = routeClient((request) async {
          expect(request.url.path, '/v1/matches');
          expect(request.method, 'GET');
          return http.Response(body, 200);
        });
        final api = TesseraApi(
          client: client,
          baseUrl: 'http://localhost:8080',
        );
        final matches = await api.listMatches();
        if (body.contains('r_1')) {
          expect(matches, hasLength(1));
          expect(matches.single.status, 'playing');
          expect(matches.single.present, 1);
        } else {
          expect(matches, isEmpty);
        }
      }
    });
  });

  group('joinMatchmaking', () {
    test('returns the paired match on 200', () async {
      Map<String, dynamic>? seenBody;
      final client = routeClient((request) async {
        expect(request.url.path, '/v1/matchmaking/join');
        final req = request as http.Request;
        seenBody = jsonDecode(req.body) as Map<String, dynamic>;
        return http.Response(
          '{"match_id":"r_ab12","seat":0,"player_id":"p_abc"}',
          200,
        );
      });
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      final paired = await api.joinMatchmaking(
        identity: _identity,
        sequencesToWin: 1,
      );
      expect(seenBody, {
        'player_id': 'p_abc',
        'token': 'p_abc.sig',
        'sequences_to_win': 1,
      });
      expect(paired, isNotNull);
      expect(paired!.matchId, 'r_ab12');
      expect(paired.seat, 0);
    });

    test('204 (left queue) becomes null, not an error', () async {
      final client = routeClient((request) async => http.Response('', 204));
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      expect(await api.joinMatchmaking(identity: _identity), isNull);
    });

    test(
      'long-poll timeout is a TimeoutException with code "timeout"',
      () async {
        final client = routeClient((request) async {
          await Future<void>.delayed(const Duration(seconds: 5));
          return http.Response('{}', 200);
        });
        final api = TesseraApi(
          client: client,
          baseUrl: 'http://localhost:8080',
        );
        await expectLater(
          api.joinMatchmaking(
            identity: _identity,
            timeout: const Duration(milliseconds: 50),
          ),
          throwsA(
            isA<TesseraApiException>().having((e) => e.code, 'code', 'timeout'),
          ),
        );
      },
    );
  });

  group('leaveMatchmaking', () {
    test('returns the server cancelled flag', () async {
      for (final body in ['{"cancelled":true}', '{"cancelled":false}']) {
        final client = routeClient((request) async {
          expect(request.url.path, '/v1/matchmaking/leave');
          return http.Response(body, 200);
        });
        final api = TesseraApi(
          client: client,
          baseUrl: 'http://localhost:8080',
        );
        expect(
          await api.leaveMatchmaking(identity: _identity),
          body.contains('true'),
        );
      }
    });
  });

  group('matchmakingStatus', () {
    test('parses the waiting count', () async {
      final client = routeClient((request) async {
        expect(request.url.path, '/v1/matchmaking/status');
        return http.Response('{"waiting":3}', 200);
      });
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      expect(await api.matchmakingStatus(), 3);
    });
  });

  group('transport behavior', () {
    test('tolerates trailing slash and missing scheme', () async {
      final client = routeClient((request) async {
        expect(request.url.path, '/v1/matchmaking/status');
        return http.Response('{"waiting":0}', 200);
      });
      for (final base in [
        'http://localhost:8080/',
        'localhost:8080',
        '  http://localhost:8080  ',
      ]) {
        final api = TesseraApi(client: client, baseUrl: base);
        expect(await api.matchmakingStatus(), 0, reason: 'baseUrl: $base');
      }
    });

    test('empty URL becomes TesseraApiException', () async {
      final client = routeClient((request) async => http.Response('{}', 200));
      final api = TesseraApi(client: client, baseUrl: '   ');
      await expectLater(
        api.matchmakingStatus(),
        throwsA(isA<TesseraApiException>()),
      );
    });

    test(
      'malformed URL becomes TesseraApiException, not FormatException',
      () async {
        final client = routeClient((request) async => http.Response('{}', 200));
        final api = TesseraApi(client: client, baseUrl: 'http://localhost:abc');
        await expectLater(
          api.matchmakingStatus(),
          throwsA(isA<TesseraApiException>()),
        );
      },
    );

    test('connection failure becomes TesseraApiException', () async {
      final client = routeClient((request) async {
        throw http.ClientException('Connection refused');
      });
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      await expectLater(api.listMatches(), throwsA(isA<TesseraApiException>()));
    });

    test('non-JSON error body still reports the status code', () async {
      final client = routeClient(
        (request) async => http.Response('Bad Gateway', 502),
      );
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      await expectLater(
        api.listMatches(),
        throwsA(
          isA<TesseraApiException>()
              .having((e) => e.code, 'code', isNull)
              .having((e) => e.statusCode, 'statusCode', 502)
              .having((e) => e.message, 'message', contains('502')),
        ),
      );
    });

    test('200 with a malformed body becomes TesseraApiException', () async {
      for (final body in ['not json', '[1,2]', '{"waiting":"many"}']) {
        final client = routeClient((request) async => http.Response(body, 200));
        final api = TesseraApi(
          client: client,
          baseUrl: 'http://localhost:8080',
        );
        await expectLater(
          api.matchmakingStatus(),
          throwsA(isA<TesseraApiException>()),
          reason: 'body: $body',
        );
      }
    });

    test('unexpected 200 on createPlayer (wants 201) is an error', () async {
      final client = routeClient(
        (request) async => http.Response('{"player_id":"p_1"}', 200),
      );
      final api = TesseraApi(client: client, baseUrl: 'http://localhost:8080');
      await expectLater(
        api.createPlayer(),
        throwsA(isA<TesseraApiException>()),
      );
    });
  });
}
