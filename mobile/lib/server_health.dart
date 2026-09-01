import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;

/// Default server origin per platform.
///
/// `localhost` on a phone points at the phone itself, not the dev machine:
/// the Android emulator aliases the host loopback as `10.0.2.2`, while the
/// iOS simulator shares the Mac's network so plain `localhost` works.
/// The field is editable regardless — this is just the starting guess.
String defaultBaseUrl(TargetPlatform platform) =>
    platform == TargetPlatform.android
    ? 'http://10.0.2.2:8080'
    : 'http://localhost:8080';

/// Snapshot of the backend's `GET /healthz` endpoint.
///
/// The backend (B0) answers 200 with JSON `{"status": "ok", "uptime": "1.234s"}`.
/// This model is the M1 slice of the wire protocol documented in
/// `docs/protocol.md`: just enough to prove the app can reach the server.
class ServerHealth {
  /// The backend's self-reported status string (B0 always sends `"ok"`).
  final String status;

  /// How long the backend process has been up, as formatted by the server
  /// (e.g. `"12.345s"`). Treated as an opaque display string: only the
  /// backend knows how it measures uptime.
  final String uptime;

  /// When this snapshot was taken, in the phone's local time.
  final DateTime checkedAt;

  const ServerHealth({
    required this.status,
    required this.uptime,
    required this.checkedAt,
  });

  /// Parses a decoded `/healthz` JSON body. Throws [FormatException] when the
  /// body does not have the expected shape, so callers can treat a 200-with-
  /// garbage the same as any other failed check.
  factory ServerHealth.fromJson(Map<String, dynamic> json, DateTime checkedAt) {
    final status = json['status'];
    final uptime = json['uptime'];
    if (status is! String || uptime is! String) {
      throw FormatException('unexpected /healthz body: $json');
    }
    return ServerHealth(status: status, uptime: uptime, checkedAt: checkedAt);
  }

  /// `true` when the backend reports the exact status string B0 defines.
  bool get isOk => status == 'ok';
}

/// Failure of a [fetchServerHealth] check.
///
/// A single exception type keeps the UI simple: the screen shows
/// [message] verbatim, whether the cause was a refused connection, a
/// timeout, a non-200 status, or a malformed body. The underlying [cause]
/// is retained for logging.
class ServerHealthException implements Exception {
  final String message;
  final Object? cause;

  const ServerHealthException(this.message, [this.cause]);

  @override
  String toString() => 'ServerHealthException: $message';
}

/// Fetches `GET <baseUrl>/healthz` and returns the parsed snapshot.
///
/// [baseUrl] is the server origin, e.g. `http://localhost:8080`; a missing
/// trailing slash is fine. The [client] is injected (rather than created
/// here) so widget/unit tests can substitute a mock.
///
/// Throws [ServerHealthException] on any failure: unreachable server,
/// [timeout], non-200 status, or a body that is not the expected JSON.
Future<ServerHealth> fetchServerHealth({
  required http.Client client,
  required String baseUrl,
  Duration timeout = const Duration(seconds: 5),
}) async {
  // URL parsing lives inside the try: a malformed URL (e.g. a non-numeric
  // port) throws FormatException from Uri.parse, and the screen only catches
  // ServerHealthException — anything else would leave it stuck on "Checking…".
  late final Uri uri;
  late final http.Response response;
  try {
    uri = _healthUri(baseUrl);
    response = await client.get(uri).timeout(timeout);
  } on ServerHealthException {
    rethrow; // e.g. the empty-URL error from _healthUri: already well-formed.
  } on TimeoutException catch (e) {
    throw ServerHealthException(
      'timed out waiting for $baseUrl (${timeout.inSeconds}s)',
      e,
    );
  } on Exception catch (e) {
    // FormatException (bad URL), ClientException, SocketException, ...
    throw ServerHealthException('could not reach $baseUrl: $e', e);
  }

  if (response.statusCode != 200) {
    throw ServerHealthException(
      'server answered ${response.statusCode} (expected 200)',
    );
  }

  try {
    final body = jsonDecode(response.body);
    if (body is! Map<String, dynamic>) {
      throw const FormatException('top-level JSON is not an object');
    }
    return ServerHealth.fromJson(body, DateTime.now());
  } on FormatException catch (e) {
    throw ServerHealthException('server sent an unreadable response: $e', e);
  }
}

/// Resolves `/healthz` against [baseUrl], tolerating a missing scheme or a
/// trailing slash in what the user typed.
Uri _healthUri(String baseUrl) {
  var normalized = baseUrl.trim();
  if (normalized.isEmpty) {
    throw const ServerHealthException('server URL is empty');
  }
  if (!normalized.contains('://')) {
    normalized = 'http://$normalized';
  }
  return Uri.parse(normalized).resolve('/healthz');
}
