import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

import 'game_state.dart';
import 'server_url.dart';

class PlayerCredentials {
  final String playerId;
  final String token;

  const PlayerCredentials({required this.playerId, required this.token});

  factory PlayerCredentials.fromJson(Map<String, dynamic> json) {
    final playerId = json['player_id'];
    final token = json['token'];
    if (playerId is! String ||
        playerId.isEmpty ||
        token is! String ||
        token.isEmpty) {
      throw const FormatException('identity is missing player_id or token');
    }
    return PlayerCredentials(playerId: playerId, token: token);
  }

  Map<String, String> toJson() => {'player_id': playerId, 'token': token};
}

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
        id.isEmpty ||
        seq is! int ||
        seq < 0 ||
        status is! String ||
        status.isEmpty ||
        players is! int ||
        players < 0 ||
        present is! int ||
        present < 0 ||
        capacity is! int ||
        capacity < 1 ||
        players > capacity ||
        present > players ||
        sequencesToWin is! int ||
        sequencesToWin < 1) {
      throw const FormatException('invalid match summary');
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

class MatchmakingResult {
  final String matchId;
  final int seat;
  final String playerId;

  const MatchmakingResult({
    required this.matchId,
    required this.seat,
    required this.playerId,
  });

  factory MatchmakingResult.fromJson(Map<String, dynamic> json) {
    final matchId = json['match_id'];
    final seat = json['seat'];
    final playerId = json['player_id'];
    if (matchId is! String ||
        matchId.isEmpty ||
        seat is! int ||
        seat < 0 ||
        seat > 1 ||
        playerId is! String ||
        playerId.isEmpty) {
      throw const FormatException('invalid matchmaking result');
    }
    return MatchmakingResult(matchId: matchId, seat: seat, playerId: playerId);
  }
}

/// A stable error shape for HTTP, timeout, connection, and protocol failures.
class TesseraApiException implements Exception {
  final String message;
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

/// Typed M2 client for identity, lobby, and matchmaking REST endpoints.
class TesseraApi {
  final http.Client client;
  final String baseUrl;
  final Duration requestTimeout;

  TesseraApi({
    required this.client,
    required String baseUrl,
    this.requestTimeout = const Duration(seconds: 10),
  }) : baseUrl = normalizeServerUrl(baseUrl);

  Future<PlayerCredentials> createPlayer() async {
    final response = await _request(
      'create an identity',
      () => client.post(serverEndpoint(baseUrl, '/v1/players')),
    );
    _expectStatus(response, 201);
    return _parse('identity', response, PlayerCredentials.fromJson);
  }

  Future<List<MatchSummary>> listMatches() async {
    final response = await _request(
      'load matches',
      () => client.get(serverEndpoint(baseUrl, '/v1/matches')),
    );
    _expectStatus(response, 200);
    final body = _object(response, 'match list');
    final matches = body['matches'];
    if (matches is! List) {
      throw const TesseraApiException('server sent an invalid match list');
    }
    try {
      return matches
          .map((item) => MatchSummary.fromJson(_asObject(item)))
          .toList(growable: false);
    } on FormatException catch (error) {
      throw TesseraApiException(
        'server sent an invalid match list',
        cause: error,
      );
    }
  }

  Future<MatchSummary> createMatch(
    PlayerCredentials credentials, {
    required int sequencesToWin,
  }) async {
    final response = await _postJson('/v1/matches', {
      ...credentials.toJson(),
      'sequences_to_win': sequencesToWin,
    }, operation: 'create a match');
    _expectStatus(response, 201);
    final body = _object(response, 'created match');
    try {
      return MatchSummary.fromJson(_asObject(body['match']));
    } on FormatException catch (error) {
      throw TesseraApiException(
        'server sent an invalid created match',
        cause: error,
      );
    }
  }

  /// Loads one authoritative, per-viewer snapshot for the static M3 board.
  /// B6 requires the token in the query string, so non-local deployments must
  /// use HTTPS. The token is never stored in or rendered by the board widgets.
  Future<MatchSnapshot> getMatchState(
    String matchId,
    PlayerCredentials credentials,
  ) async {
    if (matchId.trim().isEmpty) {
      throw const TesseraApiException('match id is empty');
    }
    final endpoint = serverEndpoint(
      baseUrl,
      '/v1/matches/${Uri.encodeComponent(matchId)}',
    ).replace(queryParameters: credentials.toJson());
    final response = await _request(
      'load match state',
      () => client.get(endpoint),
    );
    _expectStatus(response, 200);
    final snapshot = _parse('match state', response, MatchSnapshot.fromJson);
    if (snapshot.state.matchId != matchId) {
      throw const TesseraApiException(
        'server returned state for another match',
      );
    }
    return snapshot;
  }

  Future<int> matchmakingStatus() async {
    final response = await _request(
      'load matchmaking status',
      () => client.get(serverEndpoint(baseUrl, '/v1/matchmaking/status')),
    );
    _expectStatus(response, 200);
    final waiting = _object(response, 'matchmaking status')['waiting'];
    if (waiting is! int || waiting < 0) {
      throw const TesseraApiException('server sent invalid matchmaking status');
    }
    return waiting;
  }

  /// Waits for matchmaking. Completing [abortTrigger] cancels the HTTP request
  /// and therefore removes this player from the server's in-memory queue.
  /// A `null` result is the normal response to an explicit queue leave.
  Future<MatchmakingResult?> joinMatchmaking(
    PlayerCredentials credentials, {
    required int sequencesToWin,
    Future<void>? abortTrigger,
  }) async {
    final request =
        http.AbortableRequest(
            'POST',
            serverEndpoint(baseUrl, '/v1/matchmaking/join'),
            abortTrigger: abortTrigger,
          )
          ..headers['content-type'] = 'application/json'
          ..body = jsonEncode({
            ...credentials.toJson(),
            'sequences_to_win': sequencesToWin,
          });

    late final http.Response response;
    try {
      response = await http.Response.fromStream(await client.send(request));
    } on http.RequestAbortedException catch (error) {
      throw TesseraApiException(
        'matchmaking search was cancelled',
        code: 'request_aborted',
        cause: error,
      );
    } on Exception catch (error) {
      throw TesseraApiException(
        'could not search for a match: $error',
        cause: error,
      );
    }

    if (response.statusCode == 204) return null;
    _expectStatus(response, 200);
    final result = _parse(
      'matchmaking result',
      response,
      MatchmakingResult.fromJson,
    );
    if (result.playerId != credentials.playerId) {
      throw const TesseraApiException(
        'server returned matchmaking details for another player',
      );
    }
    return result;
  }

  Future<bool> leaveMatchmaking(PlayerCredentials credentials) async {
    final response = await _postJson(
      '/v1/matchmaking/leave',
      credentials.toJson(),
      operation: 'leave matchmaking',
    );
    _expectStatus(response, 200);
    final cancelled = _object(response, 'leave result')['cancelled'];
    if (cancelled is! bool) {
      throw const TesseraApiException('server sent an invalid leave result');
    }
    return cancelled;
  }

  Future<http.Response> _postJson(
    String path,
    Map<String, Object> body, {
    required String operation,
  }) => _request(
    operation,
    () => client.post(
      serverEndpoint(baseUrl, path),
      headers: const {'content-type': 'application/json'},
      body: jsonEncode(body),
    ),
  );

  Future<http.Response> _request(
    String operation,
    Future<http.Response> Function() send,
  ) async {
    try {
      return await send().timeout(requestTimeout);
    } on TimeoutException catch (error) {
      throw TesseraApiException(
        'timed out trying to $operation',
        code: 'timeout',
        cause: error,
      );
    } on TesseraApiException {
      rethrow;
    } on Exception catch (error) {
      throw TesseraApiException('could not $operation: $error', cause: error);
    }
  }

  void _expectStatus(http.Response response, int expected) {
    if (response.statusCode == expected) return;
    String? code;
    String? message;
    try {
      final error = _asObject(_asObject(jsonDecode(response.body))['error']);
      code = error['code'] as String?;
      message = error['message'] as String?;
    } on Object {
      // A proxy or old server may return plain text. The status still gives
      // callers a useful failure without leaking an HTML error page into UI.
    }
    throw TesseraApiException(
      message ?? 'server answered ${response.statusCode}',
      code: code,
      statusCode: response.statusCode,
    );
  }

  T _parse<T>(
    String label,
    http.Response response,
    T Function(Map<String, dynamic>) parse,
  ) {
    try {
      return parse(_object(response, label));
    } on FormatException catch (error) {
      throw TesseraApiException('server sent an invalid $label', cause: error);
    }
  }

  Map<String, dynamic> _object(http.Response response, String label) {
    try {
      return _asObject(jsonDecode(response.body));
    } on FormatException catch (error) {
      throw TesseraApiException('server sent an invalid $label', cause: error);
    }
  }
}

Map<String, dynamic> _asObject(Object? value) {
  if (value is! Map<String, dynamic>) {
    throw const FormatException('JSON value is not an object');
  }
  return value;
}
