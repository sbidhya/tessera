import 'package:flutter_test/flutter_test.dart';
import 'package:tessera/game_state.dart';

import 'support/match_fixture.dart';

void main() {
  test('parses a complete authoritative match snapshot', () {
    final snapshot = MatchSnapshot.fromJson(matchSnapshotJson());

    expect(snapshot.seq, 8);
    expect(snapshot.state.matchId, 'r_board');
    expect(snapshot.state.board, hasLength(100));
    expect(snapshot.state.chips, hasLength(6));
    expect(snapshot.state.viewer, 0);
    expect(snapshot.state.hand, hasLength(2));
    expect(
      snapshot.state.boardByPosition[const BoardPosition(0, 1)]!.card!.isRed,
      isTrue,
    );
    expect(
      snapshot.state.chipsByPosition[const BoardPosition(5, 5)]!.inSequence,
      isTrue,
    );
  });

  test('card helpers expose display-safe suit details', () {
    final heart = PlayingCard.fromJson({'rank': 'A', 'suit': 'hearts'});
    expect(heart.suitSymbol, '♥');
    expect(heart.isRed, isTrue);
    expect(
      () => PlayingCard.fromJson({'rank': '1', 'suit': 'stars'}),
      throwsFormatException,
    );
  });

  test('rejects partial boards and duplicate chip positions', () {
    final partial = matchSnapshotJson();
    final partialState = partial['state']! as Map<String, Object?>;
    (partialState['board']! as List<Object?>).removeLast();
    expect(() => MatchSnapshot.fromJson(partial), throwsFormatException);

    final duplicate = matchSnapshotJson();
    final duplicateState = duplicate['state']! as Map<String, Object?>;
    final chips = duplicateState['chips']! as List<Object?>;
    chips.add(chips.first);
    expect(() => MatchSnapshot.fromJson(duplicate), throwsFormatException);
  });

  test('rejects a private hand in a spectator snapshot', () {
    final json = matchSnapshotJson(spectator: true);
    final state = json['state']! as Map<String, Object?>;
    state['hand'] = [
      {'rank': 'A', 'suit': 'spades'},
    ];

    expect(() => MatchSnapshot.fromJson(json), throwsFormatException);
  });

  test('accepts a drawn terminal state without a winner', () {
    final snapshot = MatchSnapshot.fromJson(matchSnapshotJson(status: 'drawn'));

    expect(snapshot.state.status, 'drawn');
    expect(snapshot.state.winner, isNull);
  });
}
