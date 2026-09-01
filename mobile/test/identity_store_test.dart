import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/identity_store.dart';
import 'package:tessera/lobby_api.dart';

class MemoryCredentialStore implements CredentialStore {
  final Map<String, PlayerCredentials> values = {};
  Object? failure;

  @override
  Future<PlayerCredentials?> read(String serverUrl) async {
    if (failure case final Object error) throw error;
    return values[serverUrl];
  }

  @override
  Future<void> write(String serverUrl, PlayerCredentials credentials) async {
    if (failure case final Object error) throw error;
    values[serverUrl] = credentials;
  }

  @override
  Future<void> delete(String serverUrl) async {
    if (failure case final Object error) throw error;
    values.remove(serverUrl);
  }
}

void main() {
  test('returns an existing identity without calling the server', () async {
    final store = MemoryCredentialStore()
      ..values['http://server.test'] = const PlayerCredentials(
        playerId: 'p_saved',
        token: 'saved-proof',
      );
    final api = TesseraApi(
      baseUrl: 'http://server.test/path',
      client: MockClient((request) async {
        fail('server must not be called for a saved identity');
      }),
    );

    final result = await IdentityRepository(store).loadOrCreate(api);
    expect(result.playerId, 'p_saved');
  });

  test(
    'issues once, saves the pair, and reuses it on the next launch',
    () async {
      var calls = 0;
      final store = MemoryCredentialStore();
      final api = TesseraApi(
        baseUrl: 'server.test',
        client: MockClient((request) async {
          calls++;
          return http.Response(
            '{"player_id":"p_new","token":"new-proof"}',
            201,
          );
        }),
      );
      final repository = IdentityRepository(store);

      final first = await repository.loadOrCreate(api);
      final second = await repository.loadOrCreate(api);
      expect(first.playerId, 'p_new');
      expect(second.token, 'new-proof');
      expect(calls, 1);
      expect(store.values.keys, ['http://server.test']);
    },
  );

  test('server origins keep independent identities', () async {
    var issue = 0;
    final store = MemoryCredentialStore();
    TesseraApi api(String url) => TesseraApi(
      baseUrl: url,
      client: MockClient((request) async {
        issue++;
        return http.Response(
          '{"player_id":"p_$issue","token":"proof-$issue"}',
          201,
        );
      }),
    );
    final repository = IdentityRepository(store);

    await repository.loadOrCreate(api('http://one.test'));
    await repository.loadOrCreate(api('http://two.test'));
    expect(
      store.values.keys,
      containsAll(['http://one.test', 'http://two.test']),
    );
  });

  test('storage failures become a stable API-facing error', () async {
    final store = MemoryCredentialStore()
      ..failure = StateError('keychain locked');
    final api = TesseraApi(
      baseUrl: 'http://server.test',
      client: MockClient((request) async => http.Response('', 500)),
    );

    await expectLater(
      IdentityRepository(store).loadOrCreate(api),
      throwsA(
        isA<TesseraApiException>()
            .having((error) => error.code, 'code', 'secure_storage')
            .having(
              (error) => error.message,
              'message',
              contains('keychain locked'),
            ),
      ),
    );
  });
}
