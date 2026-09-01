import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/health_screen.dart';
import 'package:tessera/server_health.dart';

/// M1 unit tests: the /healthz client against canned HTTP responses.
///
/// Why a mock client instead of a live server: these tests must run on CI
/// with no backend up. The live-server check is a manual gate (see
/// mobile/README.md); the parsing/error-mapping contract is pinned here.

http.Client okClient() => MockClient((request) async {
  expect(request.url.path, '/healthz');
  return http.Response('{"status":"ok","uptime":"12.345s"}', 200);
});

void main() {
  group('ServerHealth.fromJson', () {
    test('parses the B0 body', () {
      final h = ServerHealth.fromJson({
        'status': 'ok',
        'uptime': '1.002s',
      }, DateTime.utc(2026, 1, 1));
      expect(h.status, 'ok');
      expect(h.uptime, '1.002s');
      expect(h.isOk, isTrue);
    });

    test('non-ok status is kept, not normalized away', () {
      final h = ServerHealth.fromJson({
        'status': 'degraded',
        'uptime': '0s',
      }, DateTime.now());
      expect(h.isOk, isFalse);
    });

    test('rejects missing/wrong-typed fields', () {
      expect(
        () => ServerHealth.fromJson({'status': 'ok'}, DateTime.now()),
        throwsFormatException,
      );
      expect(
        () => ServerHealth.fromJson({
          'status': 'ok',
          'uptime': 42,
        }, DateTime.now()),
        throwsFormatException,
      );
    });
  });

  group('fetchServerHealth', () {
    test('returns parsed health on 200', () async {
      final h = await fetchServerHealth(
        client: okClient(),
        baseUrl: 'http://localhost:8080',
      );
      expect(h.status, 'ok');
      expect(h.uptime, '12.345s');
      expect(h.isOk, isTrue);
    });

    test('tolerates trailing slash and missing scheme', () async {
      for (final base in [
        'http://localhost:8080/',
        'localhost:8080',
        '  http://localhost:8080  ',
      ]) {
        final h = await fetchServerHealth(client: okClient(), baseUrl: base);
        expect(h.isOk, isTrue, reason: 'baseUrl: $base');
      }
    });

    test('non-200 becomes ServerHealthException', () async {
      final client = MockClient((request) async => http.Response('oops', 500));
      expect(
        fetchServerHealth(client: client, baseUrl: 'http://localhost:8080'),
        throwsA(
          isA<ServerHealthException>().having(
            (e) => e.message,
            'message',
            contains('500'),
          ),
        ),
      );
    });

    test('200 with malformed body becomes ServerHealthException', () async {
      for (final body in ['not json', '[1,2]', '{"status":42}']) {
        final client = MockClient((request) async => http.Response(body, 200));
        await expectLater(
          fetchServerHealth(client: client, baseUrl: 'http://localhost:8080'),
          throwsA(isA<ServerHealthException>()),
          reason: 'body: $body',
        );
      }
    });

    test('connection failure becomes ServerHealthException', () async {
      final client = MockClient((request) async {
        throw http.ClientException('Connection refused');
      });
      await expectLater(
        fetchServerHealth(client: client, baseUrl: 'http://localhost:8080'),
        throwsA(isA<ServerHealthException>()),
      );
    });

    test('timeout becomes ServerHealthException', () async {
      final client = MockClient((request) async {
        await Future<void>.delayed(const Duration(seconds: 5));
        return http.Response('{}', 200);
      });
      await expectLater(
        fetchServerHealth(
          client: client,
          baseUrl: 'http://localhost:8080',
          timeout: const Duration(milliseconds: 50),
        ),
        throwsA(
          isA<ServerHealthException>().having(
            (e) => e.message,
            'message',
            contains('timed out'),
          ),
        ),
      );
    });

    test('empty URL becomes ServerHealthException', () async {
      await expectLater(
        fetchServerHealth(client: okClient(), baseUrl: '   '),
        throwsA(isA<ServerHealthException>()),
      );
    });

    test(
      'malformed URL becomes ServerHealthException, not FormatException',
      () async {
        // Uri.parse throws FormatException on a non-numeric port; the screen
        // only catches ServerHealthException, so anything else would strand it
        // on "Checking…".
        await expectLater(
          fetchServerHealth(
            client: okClient(),
            baseUrl: 'http://localhost:abc',
          ),
          throwsA(isA<ServerHealthException>()),
        );
      },
    );
  });

  group('defaultBaseUrl', () {
    test('android uses the emulator host-loopback alias', () {
      expect(defaultBaseUrl(TargetPlatform.android), 'http://10.0.2.2:8080');
    });

    test('iOS uses plain localhost (simulator shares Mac network)', () {
      expect(defaultBaseUrl(TargetPlatform.iOS), 'http://localhost:8080');
    });
  });
}
