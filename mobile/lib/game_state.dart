const sequenceBoardSize = 10;

const _validRanks = {
  'A',
  '2',
  '3',
  '4',
  '5',
  '6',
  '7',
  '8',
  '9',
  '10',
  'J',
  'Q',
  'K',
};
const _validSuits = {'spades', 'hearts', 'diamonds', 'clubs'};
const _validStatuses = {'waiting', 'playing', 'finished'};

/// One card in the backend's stable, human-readable wire format.
class PlayingCard {
  final String rank;
  final String suit;

  const PlayingCard({required this.rank, required this.suit});

  factory PlayingCard.fromJson(Map<String, dynamic> json) {
    final rank = json['rank'];
    final suit = json['suit'];
    if (rank is! String ||
        !_validRanks.contains(rank) ||
        suit is! String ||
        !_validSuits.contains(suit)) {
      throw const FormatException('invalid card');
    }
    return PlayingCard(rank: rank, suit: suit);
  }

  String get suitSymbol => switch (suit) {
    'spades' => '♠',
    'hearts' => '♥',
    'diamonds' => '♦',
    'clubs' => '♣',
    _ => '?',
  };

  bool get isRed => suit == 'hearts' || suit == 'diamonds';

  String get spokenName => '$rank of $suit';
}

/// A value coordinate makes it safe to index chips independently of wire order.
class BoardPosition {
  final int row;
  final int col;

  const BoardPosition(this.row, this.col);

  factory BoardPosition.fromJson(Map<String, dynamic> json) {
    final row = json['row'];
    final col = json['col'];
    if (row is! int ||
        col is! int ||
        row < 0 ||
        row >= sequenceBoardSize ||
        col < 0 ||
        col >= sequenceBoardSize) {
      throw const FormatException('invalid board position');
    }
    return BoardPosition(row, col);
  }

  bool get isCorner =>
      (row == 0 || row == sequenceBoardSize - 1) &&
      (col == 0 || col == sequenceBoardSize - 1);

  @override
  bool operator ==(Object other) =>
      other is BoardPosition && row == other.row && col == other.col;

  @override
  int get hashCode => Object.hash(row, col);
}

class BoardCellState {
  final BoardPosition position;
  final bool corner;
  final PlayingCard? card;

  const BoardCellState({
    required this.position,
    required this.corner,
    required this.card,
  });

  factory BoardCellState.fromJson(Map<String, dynamic> json) {
    final position = BoardPosition.fromJson(json);
    final corner = json['corner'];
    if (corner is! bool || corner != position.isCorner) {
      throw const FormatException('invalid corner marker');
    }
    final rawCard = json['card'];
    if (corner && rawCard != null || !corner && rawCard == null) {
      throw const FormatException('invalid board card');
    }
    return BoardCellState(
      position: position,
      corner: corner,
      card: rawCard == null ? null : PlayingCard.fromJson(_asObject(rawCard)),
    );
  }
}

class BoardChipState {
  final BoardPosition position;
  final int owner;
  final bool inSequence;

  const BoardChipState({
    required this.position,
    required this.owner,
    required this.inSequence,
  });

  factory BoardChipState.fromJson(Map<String, dynamic> json, int numPlayers) {
    final position = BoardPosition.fromJson(json);
    final owner = json['owner'];
    final inSequence = json['in_sequence'];
    if (position.isCorner ||
        owner is! int ||
        owner < 0 ||
        owner >= numPlayers ||
        inSequence is! bool) {
      throw const FormatException('invalid chip');
    }
    return BoardChipState(
      position: position,
      owner: owner,
      inSequence: inSequence,
    );
  }
}

class SeatCount {
  final int seat;
  final int count;

  const SeatCount({required this.seat, required this.count});

  factory SeatCount.fromJson(Map<String, dynamic> json, int numPlayers) {
    final seat = json['seat'];
    final count = json['count'];
    if (seat is! int ||
        seat < 0 ||
        seat >= numPlayers ||
        count is! int ||
        count < 0) {
      throw const FormatException('invalid seat count');
    }
    return SeatCount(seat: seat, count: count);
  }
}

class MatchPlayer {
  final String id;
  final int seat;
  final bool present;

  const MatchPlayer({
    required this.id,
    required this.seat,
    required this.present,
  });

  factory MatchPlayer.fromJson(Map<String, dynamic> json, int numPlayers) {
    final id = json['id'];
    final seat = json['seat'];
    final present = json['present'];
    if (id is! String ||
        id.isEmpty ||
        seat is! int ||
        seat < 0 ||
        seat >= numPlayers ||
        present is! bool) {
      throw const FormatException('invalid player');
    }
    return MatchPlayer(id: id, seat: seat, present: present);
  }
}

class CompletedSequence {
  final int owner;
  final List<BoardPosition> cells;

  const CompletedSequence({required this.owner, required this.cells});

  factory CompletedSequence.fromJson(
    Map<String, dynamic> json,
    int numPlayers,
  ) {
    final owner = json['owner'];
    final rawCells = json['cells'];
    if (owner is! int ||
        owner < 0 ||
        owner >= numPlayers ||
        rawCells is! List ||
        rawCells.length != 5) {
      throw const FormatException('invalid completed sequence');
    }
    return CompletedSequence(
      owner: owner,
      cells: List.unmodifiable(
        rawCells.map((value) => BoardPosition.fromJson(_asObject(value))),
      ),
    );
  }
}

/// The authoritative game view returned by GET /v1/matches/{id}.
///
/// The decoder validates board geometry and seat references at the boundary so
/// rendering code never has to guess how to handle a partial server snapshot.
class MatchState {
  final String matchId;
  final String status;
  final int numPlayers;
  final int sequencesToWin;
  final int turn;
  final int? winner;
  final int? viewer;
  final List<PlayingCard> hand;
  final List<SeatCount> handCounts;
  final List<BoardCellState> board;
  final List<BoardChipState> chips;
  final List<CompletedSequence> sequences;
  final List<SeatCount> sequencesWon;
  final int drawRemaining;
  final List<MatchPlayer> players;

  const MatchState({
    required this.matchId,
    required this.status,
    required this.numPlayers,
    required this.sequencesToWin,
    required this.turn,
    required this.winner,
    required this.viewer,
    required this.hand,
    required this.handCounts,
    required this.board,
    required this.chips,
    required this.sequences,
    required this.sequencesWon,
    required this.drawRemaining,
    required this.players,
  });

  factory MatchState.fromJson(Map<String, dynamic> json) {
    final matchId = json['match_id'];
    final status = json['status'];
    final numPlayers = json['num_players'];
    final sequencesToWin = json['sequences_to_win'];
    final turn = json['turn'];
    final drawRemaining = json['draw_remaining'];
    if (matchId is! String ||
        matchId.isEmpty ||
        status is! String ||
        !_validStatuses.contains(status) ||
        numPlayers is! int ||
        numPlayers < 2 ||
        sequencesToWin is! int ||
        sequencesToWin < 1 ||
        turn is! int ||
        turn < 0 ||
        turn >= numPlayers ||
        drawRemaining is! int ||
        drawRemaining < 0) {
      throw const FormatException('invalid match state');
    }

    final winner = _optionalSeat(json['winner'], numPlayers);
    final viewer = _optionalSeat(json['viewer'], numPlayers);
    final hand = _parseList(
      json['hand'],
      (value) => PlayingCard.fromJson(_asObject(value)),
    );
    final handCounts = _parseList(
      json['hand_counts'],
      (value) => SeatCount.fromJson(_asObject(value), numPlayers),
    );
    final board = _parseList(
      json['board'],
      (value) => BoardCellState.fromJson(_asObject(value)),
    );
    final chips = _parseList(
      json['chips'],
      (value) => BoardChipState.fromJson(_asObject(value), numPlayers),
    );
    final sequences = _parseList(
      json['sequences'],
      (value) => CompletedSequence.fromJson(_asObject(value), numPlayers),
    );
    final sequencesWon = _parseList(
      json['sequences_won'],
      (value) => SeatCount.fromJson(_asObject(value), numPlayers),
    );
    final players = _parseList(
      json['players'],
      (value) => MatchPlayer.fromJson(_asObject(value), numPlayers),
    );

    _requireCompleteBoard(board);
    _requireUniquePositions(chips.map((chip) => chip.position), 'chips');
    _requireEverySeat(handCounts, numPlayers, 'hand counts');
    _requireEverySeat(sequencesWon, numPlayers, 'sequence counts');
    _requireUniqueSeats(players.map((player) => player.seat), 'players');

    if (viewer == null && hand.isNotEmpty) {
      throw const FormatException('spectator state exposes a hand');
    }
    if (winner != null && status != 'finished' ||
        winner == null && status == 'finished') {
      throw const FormatException('winner does not match status');
    }

    return MatchState(
      matchId: matchId,
      status: status,
      numPlayers: numPlayers,
      sequencesToWin: sequencesToWin,
      turn: turn,
      winner: winner,
      viewer: viewer,
      hand: List.unmodifiable(hand),
      handCounts: List.unmodifiable(handCounts),
      board: List.unmodifiable(board),
      chips: List.unmodifiable(chips),
      sequences: List.unmodifiable(sequences),
      sequencesWon: List.unmodifiable(sequencesWon),
      drawRemaining: drawRemaining,
      players: List.unmodifiable(players),
    );
  }

  Map<BoardPosition, BoardChipState> get chipsByPosition => {
    for (final chip in chips) chip.position: chip,
  };

  Map<BoardPosition, BoardCellState> get boardByPosition => {
    for (final cell in board) cell.position: cell,
  };
}

class MatchSnapshot {
  final int seq;
  final MatchState state;

  const MatchSnapshot({required this.seq, required this.state});

  factory MatchSnapshot.fromJson(Map<String, dynamic> json) {
    final seq = json['seq'];
    if (seq is! int || seq < 0) {
      throw const FormatException('invalid match sequence');
    }
    return MatchSnapshot(
      seq: seq,
      state: MatchState.fromJson(_asObject(json['state'])),
    );
  }
}

Map<String, dynamic> _asObject(Object? value) {
  if (value is! Map<String, dynamic>) {
    throw const FormatException('JSON value is not an object');
  }
  return value;
}

List<T> _parseList<T>(Object? value, T Function(Object?) parse) {
  if (value is! List) throw const FormatException('JSON value is not a list');
  return value.map(parse).toList(growable: false);
}

int? _optionalSeat(Object? value, int numPlayers) {
  if (value == null) return null;
  if (value is! int || value < 0 || value >= numPlayers) {
    throw const FormatException('invalid seat');
  }
  return value;
}

void _requireCompleteBoard(List<BoardCellState> board) {
  if (board.length != sequenceBoardSize * sequenceBoardSize) {
    throw const FormatException('board must contain 100 cells');
  }
  _requireUniquePositions(board.map((cell) => cell.position), 'board');
}

void _requireUniquePositions(Iterable<BoardPosition> positions, String label) {
  final seen = <BoardPosition>{};
  for (final position in positions) {
    if (!seen.add(position)) {
      throw FormatException('$label contain a duplicate position');
    }
  }
}

void _requireEverySeat(List<SeatCount> counts, int numPlayers, String label) {
  if (counts.length != numPlayers) {
    throw FormatException('$label must contain every seat');
  }
  _requireUniqueSeats(counts.map((count) => count.seat), label);
}

void _requireUniqueSeats(Iterable<int> seats, String label) {
  final seen = <int>{};
  for (final seat in seats) {
    if (!seen.add(seat)) {
      throw FormatException('$label contain a duplicate seat');
    }
  }
}
