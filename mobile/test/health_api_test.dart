import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/health_api.dart';

void main() {
  test('fetchHealth decodes the backend health response', () async {
    final client = MockClient((request) async {
      expect(request.method, 'GET');
      expect(request.url, Uri.parse('http://example.test/healthz'));
      return http.Response('{"status":"ok","uptime":"2s"}', 200);
    });
    final api = HealthApi(
      client: client,
      baseUri: Uri.parse('http://example.test'),
    );

    final health = await api.fetchHealth();

    expect(health.status, 'ok');
    expect(health.uptime, '2s');
  });

  test('fetchHealth rejects a non-successful status code', () async {
    final api = HealthApi(
      client: MockClient((_) async => http.Response('unavailable', 503)),
      baseUri: Uri.parse('http://example.test'),
    );

    await expectLater(
      api.fetchHealth(),
      throwsA(
        isA<HealthException>().having(
          (error) => error.message,
          'message',
          contains('HTTP 503'),
        ),
      ),
    );
  });

  test('fetchHealth rejects invalid JSON', () async {
    final api = HealthApi(
      client: MockClient((_) async => http.Response('not JSON', 200)),
      baseUri: Uri.parse('http://example.test'),
    );

    await expectLater(api.fetchHealth(), throwsA(isA<HealthException>()));
  });

  test('fetchHealth rejects a payload with missing fields', () async {
    final api = HealthApi(
      client: MockClient((_) async => http.Response('{"status":"ok"}', 200)),
      baseUri: Uri.parse('http://example.test'),
    );

    await expectLater(api.fetchHealth(), throwsA(isA<HealthException>()));
  });
}
