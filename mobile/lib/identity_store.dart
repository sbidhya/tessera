import 'tessera_api.dart';

/// Where the app keeps its anonymous [PlayerIdentity].
///
/// `docs/match.md` calls for secure storage: whoever holds the token owns
/// the seat, so it should live in the platform keychain/keystore rather than
/// plain preferences. M2 keeps the identity behind this tiny seam with an
/// in-memory implementation, so the UI and API layers never learn where the
/// bytes live and widget tests stay hermetic (no platform channels). Swapping
/// in `flutter_secure_storage` later means adding one class that implements
/// these three methods — no UI or networking changes.
abstract class IdentityStore {
  /// Returns the saved identity, or null when the player has none yet (or
  /// forgot it).
  Future<PlayerIdentity?> load();

  /// Persists the identity, replacing any previous one.
  Future<void> save(PlayerIdentity identity);

  /// Forgets the identity. The server-side seat stays held (there is no
  /// server-side logout for anonymous ids); this just detaches the device.
  Future<void> clear();
}

/// [IdentityStore] that keeps the identity in process memory.
///
/// The identity is lost on app restart — acceptable for the M2 learning
/// gate (manual play on an emulator), and exactly what the widget tests
/// want. Production persistence is one [IdentityStore] implementation away.
class InMemoryIdentityStore implements IdentityStore {
  PlayerIdentity? _identity;

  @override
  Future<PlayerIdentity?> load() async => _identity;

  @override
  Future<void> save(PlayerIdentity identity) async {
    _identity = identity;
  }

  @override
  Future<void> clear() async {
    _identity = null;
  }
}
