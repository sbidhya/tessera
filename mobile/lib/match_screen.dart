import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'game_state.dart';
import 'lobby_api.dart';

/// M3's read-only match view. M4 will replace manual refresh with live socket
/// updates; keeping fetching outside [SequenceBoard] leaves the renderer reusable.
class MatchScreen extends StatefulWidget {
  final http.Client httpClient;
  final String baseUrl;
  final String matchId;
  final PlayerCredentials credentials;
  final Duration requestTimeout;

  const MatchScreen({
    super.key,
    required this.httpClient,
    required this.baseUrl,
    required this.matchId,
    required this.credentials,
    this.requestTimeout = const Duration(seconds: 10),
  });

  @override
  State<MatchScreen> createState() => _MatchScreenState();
}

class _MatchScreenState extends State<MatchScreen> {
  late final TesseraApi _api;
  MatchSnapshot? _snapshot;
  String? _error;
  bool _loading = true;
  int _loadGeneration = 0;

  @override
  void initState() {
    super.initState();
    _api = TesseraApi(
      client: widget.httpClient,
      baseUrl: widget.baseUrl,
      requestTimeout: widget.requestTimeout,
    );
    _load();
  }

  @override
  void dispose() {
    _loadGeneration++;
    super.dispose();
  }

  Future<void> _load() async {
    if (_loading && _snapshot != null) return;
    final generation = ++_loadGeneration;
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final snapshot = await _api.getMatchState(
        widget.matchId,
        widget.credentials,
      );
      if (!mounted || generation != _loadGeneration) return;
      setState(() => _snapshot = snapshot);
    } on Object catch (error) {
      if (!mounted || generation != _loadGeneration) return;
      setState(() => _error = _errorMessage(error));
    } finally {
      if (mounted && generation == _loadGeneration) {
        setState(() => _loading = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: Text('Match ${widget.matchId}'),
        actions: [
          IconButton(
            key: const Key('refreshMatchButton'),
            tooltip: 'Refresh match state',
            onPressed: _loading ? null : _load,
            icon: _loading && _snapshot != null
                ? const SizedBox.square(
                    dimension: 20,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                : const Icon(Icons.refresh),
          ),
        ],
      ),
      body: _buildBody(),
    );
  }

  Widget _buildBody() {
    final snapshot = _snapshot;
    if (snapshot == null && _loading) {
      return const Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            CircularProgressIndicator(),
            SizedBox(height: 16),
            Text('Loading authoritative match state…'),
          ],
        ),
      );
    }
    if (snapshot == null) {
      return Center(
        child: Padding(
          padding: const EdgeInsets.all(24),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const Icon(Icons.sync_problem, size: 48),
              const SizedBox(height: 12),
              Text(_error!, textAlign: TextAlign.center),
              const SizedBox(height: 16),
              FilledButton(
                key: const Key('retryMatchButton'),
                onPressed: _load,
                child: const Text('Retry'),
              ),
            ],
          ),
        ),
      );
    }

    final state = snapshot.state;
    return ListView(
      padding: const EdgeInsets.all(12),
      children: [
        if (_error != null) ...[
          MaterialBanner(
            key: const Key('matchRefreshError'),
            content: Text(_error!),
            actions: [
              TextButton(onPressed: _load, child: const Text('Try again')),
            ],
          ),
          const SizedBox(height: 8),
        ],
        _MatchSummaryCard(snapshot: snapshot),
        const SizedBox(height: 12),
        Center(
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 700),
            child: SequenceBoard(state: state),
          ),
        ),
        const SizedBox(height: 12),
        _PlayerLegend(state: state),
        const SizedBox(height: 8),
        Text(
          'Static REST snapshot · use refresh to reload. Live moves arrive in M4.',
          textAlign: TextAlign.center,
          style: Theme.of(context).textTheme.bodySmall,
        ),
      ],
    );
  }
}

class _MatchSummaryCard extends StatelessWidget {
  final MatchSnapshot snapshot;

  const _MatchSummaryCard({required this.snapshot});

  @override
  Widget build(BuildContext context) {
    final state = snapshot.state;
    final viewer = state.viewer;
    final headline = switch (state.status) {
      'waiting' => 'Waiting for players',
      'finished' => 'Player ${state.winner! + 1} won',
      _ when viewer == state.turn => 'Your turn',
      _ => 'Player ${state.turn + 1}\'s turn',
    };
    final view = viewer == null
        ? 'Spectator view'
        : 'You are Player ${viewer + 1} · ${state.hand.length} cards';

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Row(
          children: [
            Icon(
              state.status == 'finished' ? Icons.emoji_events : Icons.casino,
              color: state.status == 'finished'
                  ? Colors.amber.shade800
                  : Theme.of(context).colorScheme.primary,
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    headline,
                    style: Theme.of(context).textTheme.titleMedium,
                  ),
                  Text(
                    '$view · ${state.sequencesToWin} sequence${state.sequencesToWin == 1 ? '' : 's'} to win',
                  ),
                ],
              ),
            ),
            Text(
              '#${snapshot.seq}',
              key: const Key('matchSequence'),
              style: Theme.of(context).textTheme.labelLarge,
            ),
          ],
        ),
      ),
    );
  }
}

/// Pure 10×10 renderer for one authoritative [MatchState].
class SequenceBoard extends StatelessWidget {
  final MatchState state;

  const SequenceBoard({super.key, required this.state});

  @override
  Widget build(BuildContext context) {
    final cells = state.boardByPosition;
    final chips = state.chipsByPosition;
    return AspectRatio(
      aspectRatio: 1,
      child: DecoratedBox(
        decoration: BoxDecoration(
          color: const Color(0xff244f3d),
          border: Border.all(color: const Color(0xff17362a), width: 3),
          borderRadius: BorderRadius.circular(8),
        ),
        child: Padding(
          padding: const EdgeInsets.all(3),
          child: GridView.builder(
            key: const Key('sequenceBoard'),
            physics: const NeverScrollableScrollPhysics(),
            padding: EdgeInsets.zero,
            gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
              crossAxisCount: sequenceBoardSize,
              crossAxisSpacing: 2,
              mainAxisSpacing: 2,
            ),
            itemCount: sequenceBoardSize * sequenceBoardSize,
            itemBuilder: (context, index) {
              final position = BoardPosition(
                index ~/ sequenceBoardSize,
                index % sequenceBoardSize,
              );
              return SequenceBoardCell(
                cell: cells[position]!,
                chip: chips[position],
              );
            },
          ),
        ),
      ),
    );
  }
}

class SequenceBoardCell extends StatelessWidget {
  final BoardCellState cell;
  final BoardChipState? chip;

  const SequenceBoardCell({super.key, required this.cell, required this.chip});

  @override
  Widget build(BuildContext context) {
    final position = cell.position;
    final label = cell.corner
        ? 'Row ${position.row + 1}, column ${position.col + 1}, free corner'
        : 'Row ${position.row + 1}, column ${position.col + 1}, ${cell.card!.spokenName}'
              '${chip == null ? '' : ', Player ${chip!.owner + 1} chip'}'
              '${chip?.inSequence == true ? ', completed sequence' : ''}';
    return Semantics(
      label: label,
      container: true,
      child: Container(
        key: ValueKey('board-cell-${position.row}-${position.col}'),
        decoration: BoxDecoration(
          color: cell.corner ? const Color(0xffffd86b) : Colors.white,
          borderRadius: BorderRadius.circular(3),
        ),
        child: Stack(
          fit: StackFit.expand,
          children: [
            if (cell.corner)
              Center(
                child: FittedBox(
                  key: ValueKey('corner-${position.row}-${position.col}'),
                  child: const Padding(
                    padding: EdgeInsets.all(4),
                    child: Icon(Icons.star, color: Color(0xff795500)),
                  ),
                ),
              )
            else
              PlayingCardFace(
                key: ValueKey('card-${position.row}-${position.col}'),
                card: cell.card!,
              ),
            if (chip != null)
              Center(
                child: FractionallySizedBox(
                  widthFactor: 0.72,
                  heightFactor: 0.72,
                  child: Container(
                    key: ValueKey(
                      '${chip!.inSequence ? 'sequence-chip' : 'chip'}-'
                      '${position.row}-${position.col}',
                    ),
                    decoration: BoxDecoration(
                      shape: BoxShape.circle,
                      color: playerColor(chip!.owner),
                      border: Border.all(
                        color: chip!.inSequence
                            ? const Color(0xffffd54f)
                            : Colors.white70,
                        width: chip!.inSequence ? 3 : 1,
                      ),
                      boxShadow: const [
                        BoxShadow(
                          color: Colors.black38,
                          blurRadius: 2,
                          offset: Offset(0, 1),
                        ),
                      ],
                    ),
                  ),
                ),
              ),
          ],
        ),
      ),
    );
  }
}

class PlayingCardFace extends StatelessWidget {
  final PlayingCard card;

  const PlayingCardFace({super.key, required this.card});

  @override
  Widget build(BuildContext context) {
    final color = card.isRed ? const Color(0xffb3261e) : Colors.black87;
    return Padding(
      padding: const EdgeInsets.all(2),
      child: FittedBox(
        fit: BoxFit.scaleDown,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(
              card.rank,
              style: TextStyle(
                color: color,
                fontSize: 14,
                fontWeight: FontWeight.w800,
                height: 0.9,
              ),
            ),
            Text(
              card.suitSymbol,
              style: TextStyle(color: color, fontSize: 14, height: 0.9),
            ),
          ],
        ),
      ),
    );
  }
}

class _PlayerLegend extends StatelessWidget {
  final MatchState state;

  const _PlayerLegend({required this.state});

  @override
  Widget build(BuildContext context) {
    if (state.players.isEmpty) {
      return const Center(child: Text('No seats claimed yet'));
    }
    return Wrap(
      alignment: WrapAlignment.center,
      spacing: 16,
      runSpacing: 8,
      children: [
        for (final player in state.players)
          Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              Container(
                width: 16,
                height: 16,
                decoration: BoxDecoration(
                  shape: BoxShape.circle,
                  color: playerColor(player.seat),
                ),
              ),
              const SizedBox(width: 6),
              Text(
                'Player ${player.seat + 1}${state.viewer == player.seat ? ' (you)' : ''} '
                '· ${player.present ? 'present' : 'away'}',
              ),
            ],
          ),
      ],
    );
  }
}

Color playerColor(int seat) => switch (seat % 4) {
  0 => const Color(0xff3155a6),
  1 => const Color(0xffd55220),
  2 => const Color(0xff7b3fa1),
  _ => const Color(0xff008577),
};

String _errorMessage(Object error) => switch (error) {
  TesseraApiException() => error.message,
  _ => 'Unexpected match error: $error',
};
