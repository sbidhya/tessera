import 'dart:convert';

import 'package:http/http.dart' as http;

Map<String, Object?> matchSnapshotJson({
  String matchId = 'r_board',
  int seq = 8,
  bool spectator = false,
}) {
  final board = <Map<String, Object?>>[];
  for (var row = 0; row < 10; row++) {
    for (var col = 0; col < 10; col++) {
      final corner = (row == 0 || row == 9) && (col == 0 || col == 9);
      board.add({
        'row': row,
        'col': col,
        'corner': corner,
        if (!corner)
          'card': {
            'rank': row == 0 && col == 1 ? 'A' : '10',
            'suit': row == 0 && col == 1 ? 'hearts' : 'clubs',
          },
      });
    }
  }

  return {
    'seq': seq,
    'state': {
      'match_id': matchId,
      'status': 'playing',
      'num_players': 2,
      'sequences_to_win': 2,
      'turn': 0,
      'winner': null,
      'viewer': spectator ? null : 0,
      'hand': spectator
          ? <Object>[]
          : [
              {'rank': 'J', 'suit': 'diamonds'},
              {'rank': 'Q', 'suit': 'spades'},
            ],
      'hand_counts': [
        {'seat': 0, 'count': 2},
        {'seat': 1, 'count': 7},
      ],
      'board': board,
      'chips': [
        {'row': 0, 'col': 1, 'owner': 0, 'in_sequence': false},
        for (var col = 1; col <= 5; col++)
          {'row': 5, 'col': col, 'owner': 1, 'in_sequence': true},
      ],
      'sequences': [
        {
          'owner': 1,
          'cells': [
            for (var col = 1; col <= 5; col++) {'row': 5, 'col': col},
          ],
        },
      ],
      'sequences_won': [
        {'seat': 0, 'count': 0},
        {'seat': 1, 'count': 1},
      ],
      'draw_remaining': 80,
      'players': [
        {'id': 'p_alice', 'seat': 0, 'present': true},
        {'id': 'p_bob', 'seat': 1, 'present': false},
      ],
    },
  };
}

http.Response matchSnapshotResponse({
  String matchId = 'r_board',
  int seq = 8,
  bool spectator = false,
  int statusCode = 200,
}) => http.Response(
  jsonEncode(
    matchSnapshotJson(matchId: matchId, seq: seq, spectator: spectator),
  ),
  statusCode,
  headers: const {'content-type': 'application/json'},
);
