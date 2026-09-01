import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

/// M2 REST client: identity + lobby (create/join/matchmake).
///
/// Covers the B6 HTTP surfaces documented in `docs/protocol.md`:
/// `POST /v1/players`, `POST|GET /v1/matches`, and
/// `POST /v1/matchmaking/join|leave` + `GET /v1/matchmaking/status`.
/// Board state (`GET state`) and WebSocket play arrive in M3/M4.
///
/// Design notes (mirroring the M1 `server_health.dart` style):
/// - One exception type, [TesseraApiException], carrying the server's stable
///   `code` (e.g. `auth_disabled`, `invalid_token`) so the UI can branch on
///   it without string-matching messages.
/// - A small [TesseraApi] class instead of free functions: the endpoints
///   share base-URL normalization, and one object is easier to hand to the
///   lobby screen than six function parameters.
/// - The backend rejects unknown JSON fields (`DisallowUnknownFields`), so
///   request bodies send exactly the fields the server defines — no extras.
class TesseraApi {
  /// Injected HTTP client (a mock in tests, a real `http.Client` in prod).
  final http.Client client;

  /// Server origin, e.g. `http://localhost:8080`. Normalized per call, so a
  /// missing scheme or trailing slash in user input is tolerated.
  final String baseUrl;

  const TesseraApi({required this.client, required this.baseUrl});

  /// Mints a fresh anonymous identity: `POST /v1/players` (no body needed).
  ///
  /// The server answers `201 {"player_id": "p_…", "token": "p_….…"}`. Keep
  /// both halves: the id names the player, the token proves it. Losing the
  /// token loses the identity — there is no recovery, by design.
  ///
  /// Throws [TesseraApiException] with code `auth_disabled` when the server
  /// runs without the identity layer.
  Future<PlayerIdentity> createPlayer({
    Duration timeout = const Duration(seconds: 10),
  }) async {
    final response = await _post(
      '/v1/players',
      null,
      timeout: timeout,
      action: 'create player identity',
    );
    _requireStatus(response, const [201], 'create player identity');
    return PlayerIdentity.fromJson(
      _decodeObject(response.body, 'create player identity'),
    );
  }

  /// Creates a match: `POST /v1/matches` → `201 {"match": {...}}`.
  ///
  /// [sequencesToWin] defaults to 2 (the server default); pass 1 for a fast
  /// test game. A negative value is rejected by the server with code
  /// `invalid_options`.
  Future<MatchSummary> createMatch({
    required PlayerIdentity identity,
    int sequencesToWin = 2,
    Duration timeout = const Duration(seconds: 10),
  }) async {
    final response = await _post(
      '/v1/matches',
      {
        'sequences_to_win': sequencesToWin,
        'player_id': identity.playerId,
        'token': identity.token,
      },
      timeout: timeout,
      action: 'create match',
    );
    _requireStatus(response, const [201], 'create match');
    final body = _decodeObject(response.body, 'create match');
    final match = body['match'];
    if (match is! Map<String, dynamic>) {
      throw TesseraApiException(
        'create match: server sent an unreadable response: $body',
      );
    }
    return MatchSummary.fromJson(match);
  }

  /// Lists matches: `GET /v1/matches` → `200 {"matches": [...]}`.
  ///
  /// Public — no identity needed, so the lobby can browse before identifying.
  Future<List<MatchSummary>> listMatches({
    Duration timeout = const Duration(seconds: 10),
  }) async {
    final response = await _get(
      '/v1/matches',
      timeout: timeout,
      action: 'list matches',
    );
    _requireStatus(response, const [200], 'list matches');
    final body = _decodeObject(response.body, 'list matches');
    final matches = body['matches'];
    if (matches is! List) {
      throw TesseraApiException(
        'list matches: server sent an unreadable response: $body',
      );
    }
    return matches.map((m) {
      if (m is! Map<String, dynamic>) {
        throw TesseraApiException(
          'list matches: server sent an unreadable response: $body',
        );
      }
      return MatchSummary.fromJson(m);
    }).toList();
  }

  /// Joins the matchmaking queue and waits for a partner (long-poll).
  ///
  /// Returns the paired match — both seats are already joined, so the match
  /// is live. Returns `null` when the wait ended without a match because the
  /// player left the queue via [leaveMatchmaking] (the server answers 204).
  ///
  /// Only waiters with equal `sequencesToWin` are paired (server-side FIFO
  /// buckets). The [timeout] is the client's own queue budget: the doc
  /// (`docs/match.md`) recommends ~30s, then retry — a retry while still
  /// queued attaches to the existing entry instead of queueing twice. A
  /// timeout surfaces as [TesseraApiException] with code `timeout`; it does
  /// NOT withdraw the player (only [leaveMatchmaking] or dropping the app
  /// does — the request context is the queue membership, and ours stays
  /// open only for [timeout]).
  Future<PairedMatch?> joinMatchmaking({
    required PlayerIdentity identity,
    int sequencesToWin = 2,
    Duration timeout = const Duration(seconds: 30),
  }) async {
    final response = await _post(
      '/v1/matchmaking/join',
      {
        'player_id': identity.playerId,
        'token': identity.token,
        'sequences_to_win': sequencesToWin,
      },
      timeout: timeout,
      action: 'join matchmaking',
      timeoutCode: 'timeout',
    );
    if (response.statusCode == 204) {
      // Left the queue while the long-poll was open: a normal outcome, not
      // an error (see docs/match.md "Cancel/paired race").
      return null;
    }
    _requireStatus(response, const [200], 'join matchmaking');
    return PairedMatch.fromJson(
      _decodeObject(response.body, 'join matchmaking'),
    );
  }

  /// Withdraws from the queue: `POST /v1/matchmaking/leave`.
  ///
  /// Returns true when a queue entry was actually cancelled. The still-open
  /// [joinMatchmaking] long-poll then completes with `null`.
  Future<bool> leaveMatchmaking({
    required PlayerIdentity identity,
    Duration timeout = const Duration(seconds: 10),
  }) async {
    final response = await _post(
      '/v1/matchmaking/leave',
      {'player_id': identity.playerId, 'token': identity.token},
      timeout: timeout,
      action: 'leave matchmaking',
    );
    _requireStatus(response, const [200], 'leave matchmaking');
    final body = _decodeObject(response.body, 'leave matchmaking');
    final cancelled = body['cancelled'];
    if (cancelled is! bool) {
      throw TesseraApiException(
        'leave matchmaking: server sent an unreadable response: $body',
      );
    }
    return cancelled;
  }

  /// Queue depth: `GET /v1/matchmaking/status` → `200 {"waiting": n}`.
  Future<int> matchmakingStatus({
    Duration timeout = const Duration(seconds: 10),
  }) async {
    final response = await _get(
      '/v1/matchmaking/status',
      timeout: timeout,
      action: 'matchmaking status',
    );
    _requireStatus(response, const [200], 'matchmaking status');
    final body = _decodeObject(response.body, 'matchmaking status');
    final waiting = body['waiting'];
    if (waiting is! int) {
      throw TesseraApiException(
        'matchmaking status: server sent an unreadable response: $body',
      );
    }
    return waiting;
  }

  // -- low-level helpers --------------------------------------------------------

  /// Resolves [path] against [baseUrl], mapping every URL failure to
  /// [TesseraApiException]: the UI only catches that type, so a raw
  /// FormatException from `Uri.parse` (e.g. a non-numeric port) would strand
  /// the screen on a spinner.
  Uri _resolveUri(String path) {
    try {
      return _apiUri(baseUrl, path);
    } on TesseraApiException {
      rethrow; // empty-URL error: already well-formed.
    } on Exception catch (e) {
      throw TesseraApiException('server URL is not usable: $e', cause: e);
    }
  }

  Future<http.Response> _get(
    String path, {
    required Duration timeout,
    required String action,
    String timeoutCode = 'timeout',
  }) async {
    final uri = _resolveUri(path);
    try {
      return await client.get(uri).timeout(timeout);
    } on TimeoutException catch (e) {
      throw TesseraApiException(
        '$action timed out after ${timeout.inSeconds}s',
        code: timeoutCode,
        cause: e,
      );
    } on Exception catch (e) {
      throw TesseraApiException('$action failed: $e', cause: e);
    }
  }

  Future<http.Response> _post(
    String path,
    Map<String, Object?>? body, {
    required Duration timeout,
    required String action,
    String timeoutCode = 'timeout',
  }) async {
    final uri = _resolveUri(path);
    try {
      return await client
          .post(
            uri,
            headers: body == null
                ? null
                : const {'Content-Type': 'application/json'},
            body: body == null ? null : jsonEncode(body),
          )
          .timeout(timeout);
    } on TimeoutException catch (e) {
      throw TesseraApiException(
        '$action timed out after ${timeout.inSeconds}s',
        code: timeoutCode,
        cause: e,
      );
    } on Exception catch (e) {
      throw TesseraApiException('$action failed: $e', cause: e);
    }
  }

  /// Throws [TesseraApiException] unless the status is expected. A
  /// non-expected status tries to surface the server's stable error `code`
  /// (e.g. `invalid_token`, `auth_disabled`) from its
  /// `{"error": {"code", "message"}}` envelope.
  void _requireStatus(
    http.Response response,
    List<int> expected,
    String action,
  ) {
    if (expected.contains(response.statusCode)) return;
    final parsed = _tryDecodeError(response.body);
    throw TesseraApiException(
      parsed == null
          ? '$action failed: server answered ${response.statusCode}'
          : '$action failed: ${parsed.message}',
      code: parsed?.code,
      statusCode: response.statusCode,
    );
  }

  Map<String, dynamic> _decodeObject(String body, String action) {
    try {
      final decoded = jsonDecode(body);
      if (decoded is! Map<String, dynamic>) {
        throw const FormatException('top-level JSON is not an object');
      }
      return decoded;
    } on FormatException catch (e) {
      throw TesseraApiException(
        '$action: server sent an unreadable response: $e',
        cause: e,
      );
    }
  }

  _ServerError? _tryDecodeError(String body) {
    try {
      final decoded = jsonDecode(body);
      if (decoded is! Map<String, dynamic>) return null;
      final error = decoded['error'];
      if (error is! Map<String, dynamic>) return null;
      final code = error['code'];
      final message = error['message'];
      if (code is! String || message is! String) return null;
      return _ServerError(code, message);
    } on FormatException {
      return null;
    }
  }
}

/// Anonymous identity issued by `POST /v1/players`.
///
/// Keep both halves. The token is the proof of the id; there is no recovery
/// flow — losing it means losing the seat (see `docs/match.md`).
class PlayerIdentity {
  final String playerId;
  final String token;

  const PlayerIdentity({required this.playerId, required this.token});

  factory PlayerIdentity.fromJson(Map<String, dynamic> json) {
    final playerId = json['player_id'];
    final token = json['token'];
    if (playerId is! String || token is! String) {
      throw TesseraApiException(
        'create player identity: server sent an unreadable response: $json',
      );
    }
    return PlayerIdentity(playerId: playerId, token: token);
  }

  Map<String, dynamic> toJson() => {'player_id': playerId, 'token': token};
}

/// One row of `GET /v1/matches` (same shape as the `match` in create).
class MatchSummary {
  final String id;
  final int seq;
  final String status;
  final int players;
  final int present;
  final int capacity;
  final int sequencesToWin;

  const MatchSummary({
    required this.id,
    required this.seq,
    required this.status,
    required this.players,
    required this.present,
    required this.capacity,
    required this.sequencesToWin,
  });

  factory MatchSummary.fromJson(Map<String, dynamic> json) {
    final id = json['id'];
    final seq = json['seq'];
    final status = json['status'];
    final players = json['players'];
    final present = json['present'];
    final capacity = json['capacity'];
    final sequencesToWin = json['sequences_to_win'];
    if (id is! String ||
        seq is! int ||
        status is! String ||
        players is! int ||
        present is! int ||
        capacity is! int ||
        sequencesToWin is! int) {
      throw TesseraApiException(
        'match: server sent an unreadable response: $json',
      );
    }
    return MatchSummary(
      id: id,
      seq: seq,
      status: status,
      players: players,
      present: present,
      capacity: capacity,
      sequencesToWin: sequencesToWin,
    );
  }
}

/// Result of `POST /v1/matchmaking/join`: the room both players are seated in.
class PairedMatch {
  final String matchId;
  final int seat;
  final String playerId;

  const PairedMatch({
    required this.matchId,
    required this.seat,
    required this.playerId,
  });

  factory PairedMatch.fromJson(Map<String, dynamic> json) {
    final matchId = json['match_id'];
    final seat = json['seat'];
    final playerId = json['player_id'];
    if (matchId is! String || seat is! int || playerId is! String) {
      throw TesseraApiException(
        'join matchmaking: server sent an unreadable response: $json',
      );
    }
    return PairedMatch(matchId: matchId, seat: seat, playerId: playerId);
  }
}

/// Failure of a [TesseraApi] call.
///
/// A single exception type keeps the UI simple: the screen shows [message]
/// verbatim and branches on [code] only where behavior differs (e.g.
/// `timeout` on the long-poll means "still queued, retry"; `auth_disabled`
/// means the server has no identity layer).
class TesseraApiException implements Exception {
  final String message;

  /// The server's stable error code (`invalid_token`, `auth_disabled`, …),
  /// or the client-side pseudo-code `timeout`, or null when the failure had
  /// no machine-readable category (unreachable server, garbage body).
  final String? code;

  final int? statusCode;
  final Object? cause;

  const TesseraApiException(
    this.message, {
    this.code,
    this.statusCode,
    this.cause,
  });

  @override
  String toString() => 'TesseraApiException: $message';
}

class _ServerError {
  final String code;
  final String message;

  _ServerError(this.code, this.message);
}

/// Resolves [path] against [baseUrl], tolerating a missing scheme or a
/// trailing slash in what the user typed. (Same normalization as M1's
/// `/healthz` helper, applied to the `/v1/…` routes.)
Uri _apiUri(String baseUrl, String path) {
  var normalized = baseUrl.trim();
  if (normalized.isEmpty) {
    throw const TesseraApiException('server URL is empty');
  }
  if (!normalized.contains('://')) {
    normalized = 'http://$normalized';
  }
  return Uri.parse(normalized).resolve(path);
}
