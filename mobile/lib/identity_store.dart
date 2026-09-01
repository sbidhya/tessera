import 'dart:convert';

import 'package:flutter_secure_storage/flutter_secure_storage.dart';

import 'lobby_api.dart';
import 'server_url.dart';

/// Storage seam: production uses the platform keychain/keystore while tests
/// use an in-memory implementation without loading a Flutter plugin.
abstract interface class CredentialStore {
  Future<PlayerCredentials?> read(String serverUrl);
  Future<void> write(String serverUrl, PlayerCredentials credentials);
  Future<void> delete(String serverUrl);
}

class SecureCredentialStore implements CredentialStore {
  final FlutterSecureStorage _storage;

  const SecureCredentialStore(this._storage);

  @override
  Future<PlayerCredentials?> read(String serverUrl) async {
    final value = await _storage.read(key: _key(serverUrl));
    if (value == null) return null;
    try {
      final decoded = jsonDecode(value);
      if (decoded is! Map<String, dynamic>) return null;
      return PlayerCredentials.fromJson(decoded);
    } on FormatException {
      // A partial/old record is unusable. The repository will replace it with
      // a new server-issued identity instead of pairing an id with bad proof.
      return null;
    }
  }

  @override
  Future<void> write(String serverUrl, PlayerCredentials credentials) =>
      _storage.write(
        key: _key(serverUrl),
        value: jsonEncode(credentials.toJson()),
      );

  @override
  Future<void> delete(String serverUrl) =>
      _storage.delete(key: _key(serverUrl));

  String _key(String serverUrl) {
    final origin = normalizeServerUrl(serverUrl);
    final scope = base64Url.encode(utf8.encode(origin)).replaceAll('=', '');
    return 'tessera.credentials.v1.$scope';
  }
}

/// Loads one identity per server origin, issuing and securely saving it once.
class IdentityRepository {
  final CredentialStore _store;

  const IdentityRepository(this._store);

  Future<PlayerCredentials> loadOrCreate(TesseraApi api) async {
    try {
      final existing = await _store.read(api.baseUrl);
      if (existing != null) return existing;

      final issued = await api.createPlayer();
      // One JSON value makes the id/token pair atomic from the app's point of
      // view; neither half is ever loaded without the other.
      await _store.write(api.baseUrl, issued);
      return issued;
    } on TesseraApiException {
      rethrow;
    } on Object catch (error) {
      throw TesseraApiException(
        'could not access secure identity storage: $error',
        code: 'secure_storage',
        cause: error,
      );
    }
  }

  Future<void> forget(TesseraApi api) async {
    try {
      await _store.delete(api.baseUrl);
    } on Object catch (error) {
      throw TesseraApiException(
        'could not clear secure identity storage: $error',
        code: 'secure_storage',
        cause: error,
      );
    }
  }
}
