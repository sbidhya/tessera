import 'dart:async';

import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'identity_store.dart';
import 'lobby_api.dart';

class ReadyMatch {
  final String matchId;
  final int? seat;
  final String source;

  const ReadyMatch({required this.matchId, required this.source, this.seat});
}

/// M2 lobby: restore an anonymous identity, create/browse rooms, and wait in
/// matchmaking. Board rendering and the socket that claims a direct room seat
/// intentionally remain M3/M4 concerns.
class LobbyScreen extends StatefulWidget {
  final http.Client httpClient;
  final CredentialStore credentialStore;
  final String baseUrl;
  final Duration matchmakingTimeout;
  final ValueChanged<ReadyMatch>? onMatchReady;

  const LobbyScreen({
    super.key,
    required this.httpClient,
    required this.credentialStore,
    required this.baseUrl,
    this.matchmakingTimeout = const Duration(seconds: 30),
    this.onMatchReady,
  });

  @override
  State<LobbyScreen> createState() => _LobbyScreenState();
}

class _LobbyScreenState extends State<LobbyScreen> {
  late final TesseraApi _api;
  late final IdentityRepository _identities;
  PlayerCredentials? _credentials;
  List<MatchSummary> _matches = const [];
  ReadyMatch? _readyMatch;
  int _waiting = 0;
  int _sequencesToWin = 2;
  bool _bootstrapping = true;
  bool _refreshing = false;
  bool _creating = false;
  bool _searching = false;
  bool _cancelling = false;
  String? _fatalError;
  String? _actionError;
  Completer<void>? _searchAbort;
  Timer? _searchTimer;
  int _searchGeneration = 0;

  @override
  void initState() {
    super.initState();
    _api = TesseraApi(client: widget.httpClient, baseUrl: widget.baseUrl);
    _identities = IdentityRepository(widget.credentialStore);
    _bootstrap();
  }

  @override
  void dispose() {
    _searchGeneration++;
    _searchTimer?.cancel();
    final abort = _searchAbort;
    if (abort != null && !abort.isCompleted) abort.complete();
    super.dispose();
  }

  Future<void> _bootstrap() async {
    if (mounted) {
      setState(() {
        _bootstrapping = true;
        _fatalError = null;
      });
    }
    try {
      final credentials = await _identities.loadOrCreate(_api);
      final results = await Future.wait<Object>([
        _api.listMatches(),
        _api.matchmakingStatus(),
      ]);
      if (!mounted) return;
      setState(() {
        _credentials = credentials;
        _matches = results[0] as List<MatchSummary>;
        _waiting = results[1] as int;
        _bootstrapping = false;
      });
    } on Object catch (error) {
      if (!mounted) return;
      setState(() {
        _fatalError = _errorMessage(error);
        _bootstrapping = false;
      });
    }
  }

  Future<void> _refreshLobby() async {
    if (_refreshing || _credentials == null) return;
    setState(() {
      _refreshing = true;
      _actionError = null;
    });
    try {
      final results = await Future.wait<Object>([
        _api.listMatches(),
        _api.matchmakingStatus(),
      ]);
      if (!mounted) return;
      setState(() {
        _matches = results[0] as List<MatchSummary>;
        _waiting = results[1] as int;
      });
    } on Object catch (error) {
      if (!mounted) return;
      setState(() => _actionError = _errorMessage(error));
    } finally {
      if (mounted) setState(() => _refreshing = false);
    }
  }

  Future<void> _createMatch() async {
    if (_creating || _searching) return;
    setState(() {
      _creating = true;
      _actionError = null;
    });
    try {
      final match = await _api.createMatch(
        _credentials!,
        sequencesToWin: _sequencesToWin,
      );
      if (!mounted) return;
      final ready = ReadyMatch(matchId: match.id, source: 'Room created');
      setState(() {
        _readyMatch = ready;
        _matches = [
          match,
          ..._matches.where((existing) => existing.id != match.id),
        ];
      });
      widget.onMatchReady?.call(ready);
    } on Object catch (error) {
      if (!mounted) return;
      setState(() => _actionError = _errorMessage(error));
    } finally {
      if (mounted) setState(() => _creating = false);
    }
  }

  Future<void> _findOpponent() async {
    if (_searching || _creating) return;
    final generation = ++_searchGeneration;
    final abort = Completer<void>();
    _searchAbort = abort;
    _searchTimer = Timer(widget.matchmakingTimeout, () {
      if (!abort.isCompleted) abort.complete();
    });
    setState(() {
      _searching = true;
      _cancelling = false;
      _actionError = null;
      _readyMatch = null;
    });

    try {
      final result = await _api.joinMatchmaking(
        _credentials!,
        sequencesToWin: _sequencesToWin,
        abortTrigger: abort.future,
      );
      if (!mounted || generation != _searchGeneration) return;
      if (result == null) {
        setState(() => _actionError = 'Matchmaking was cancelled.');
        return;
      }
      final ready = ReadyMatch(
        matchId: result.matchId,
        seat: result.seat,
        source: 'Opponent found',
      );
      setState(() => _readyMatch = ready);
      widget.onMatchReady?.call(ready);
    } on TesseraApiException catch (error) {
      if (!mounted || generation != _searchGeneration) return;
      setState(() {
        _actionError = error.code == 'request_aborted'
            ? 'No opponent found in ${widget.matchmakingTimeout.inSeconds}s. Try again.'
            : error.message;
      });
    } finally {
      _searchTimer?.cancel();
      if (mounted && generation == _searchGeneration) {
        setState(() {
          _searching = false;
          _cancelling = false;
          _searchAbort = null;
        });
        unawaited(_refreshLobby());
      }
    }
  }

  Future<void> _cancelSearch() async {
    if (!_searching || _cancelling) return;
    final abort = _searchAbort;
    setState(() {
      _cancelling = true;
      _actionError = null;
    });
    try {
      final cancelled = await _api.leaveMatchmaking(_credentials!);
      if (!mounted || !_searching) return;
      if (!cancelled) {
        // `false` most often means pairing won the cancel race. Preserve the
        // join request so its response can deliver that match. It can also
        // mean the join has not reached the queue yet; in that rare case the
        // button becomes available again and a second cancel will remove it.
        setState(() {
          _cancelling = false;
          _actionError =
              'Pairing may already be finishing; waiting for match details…';
        });
        return;
      }
      _searchGeneration++;
      _searchTimer?.cancel();
      if (abort != null && !abort.isCompleted) abort.complete();
      setState(() {
        _searching = false;
        _cancelling = false;
        _searchAbort = null;
      });
      unawaited(_refreshLobby());
    } on Object catch (error) {
      _searchGeneration++;
      _searchTimer?.cancel();
      if (abort != null && !abort.isCompleted) abort.complete();
      if (mounted) {
        setState(() {
          _searching = false;
          _cancelling = false;
          _searchAbort = null;
          _actionError = _errorMessage(error);
        });
      }
    }
  }

  void _chooseMatch(MatchSummary match) {
    final ready = ReadyMatch(matchId: match.id, source: 'Room selected');
    setState(() {
      _readyMatch = ready;
      _actionError = null;
    });
    widget.onMatchReady?.call(ready);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Tessera — Lobby'),
        actions: [
          IconButton(
            key: const Key('refreshLobbyButton'),
            tooltip: 'Refresh lobby',
            onPressed: _bootstrapping || _refreshing ? null : _refreshLobby,
            icon: _refreshing
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
    if (_bootstrapping) {
      return const Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            CircularProgressIndicator(),
            SizedBox(height: 16),
            Text('Loading your player identity…'),
          ],
        ),
      );
    }
    if (_fatalError != null) {
      return Center(
        child: Padding(
          padding: const EdgeInsets.all(24),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const Icon(Icons.cloud_off, size: 48),
              const SizedBox(height: 12),
              Text(_fatalError!, textAlign: TextAlign.center),
              const SizedBox(height: 16),
              FilledButton(
                key: const Key('retryLobbyButton'),
                onPressed: _bootstrap,
                child: const Text('Retry'),
              ),
            ],
          ),
        ),
      );
    }

    return ListView(
      padding: const EdgeInsets.all(16),
      children: [
        Card(
          child: ListTile(
            leading: const Icon(Icons.person_outline),
            title: const Text('Anonymous player'),
            subtitle: Text(
              '${_credentials!.playerId}\n${_api.baseUrl}',
              key: const Key('playerIdentity'),
            ),
            isThreeLine: true,
          ),
        ),
        const SizedBox(height: 12),
        Card(
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Text('Play', style: Theme.of(context).textTheme.titleLarge),
                const SizedBox(height: 4),
                Text('$_waiting player${_waiting == 1 ? '' : 's'} waiting'),
                const SizedBox(height: 12),
                SegmentedButton<int>(
                  key: const Key('sequenceSelector'),
                  segments: const [
                    ButtonSegment(value: 1, label: Text('Quick · 1 sequence')),
                    ButtonSegment(value: 2, label: Text('Full · 2 sequences')),
                  ],
                  selected: {_sequencesToWin},
                  onSelectionChanged: _searching || _creating
                      ? null
                      : (selection) {
                          setState(() => _sequencesToWin = selection.single);
                        },
                ),
                const SizedBox(height: 12),
                if (_searching) ...[
                  const LinearProgressIndicator(),
                  const SizedBox(height: 8),
                  Text(
                    _cancelling ? 'Leaving queue…' : 'Looking for an opponent…',
                    textAlign: TextAlign.center,
                  ),
                  const SizedBox(height: 8),
                  OutlinedButton(
                    key: const Key('cancelMatchmakingButton'),
                    onPressed: _cancelling ? null : _cancelSearch,
                    child: const Text('Cancel search'),
                  ),
                ] else ...[
                  FilledButton.icon(
                    key: const Key('findOpponentButton'),
                    onPressed: _creating ? null : _findOpponent,
                    icon: const Icon(Icons.group),
                    label: const Text('Find opponent'),
                  ),
                  const SizedBox(height: 8),
                  OutlinedButton.icon(
                    key: const Key('createMatchButton'),
                    onPressed: _creating ? null : _createMatch,
                    icon: _creating
                        ? const SizedBox.square(
                            dimension: 18,
                            child: CircularProgressIndicator(strokeWidth: 2),
                          )
                        : const Icon(Icons.add),
                    label: Text(_creating ? 'Creating…' : 'Create room'),
                  ),
                ],
              ],
            ),
          ),
        ),
        if (_actionError != null) ...[
          const SizedBox(height: 12),
          MaterialBanner(
            key: const Key('lobbyError'),
            content: Text(_actionError!),
            actions: [
              TextButton(
                onPressed: () => setState(() => _actionError = null),
                child: const Text('Dismiss'),
              ),
            ],
          ),
        ],
        if (_readyMatch != null) ...[
          const SizedBox(height: 12),
          Card(
            key: const Key('readyMatchCard'),
            color: Colors.green.shade50,
            child: ListTile(
              leading: Icon(Icons.check_circle, color: Colors.green.shade700),
              title: Text(_readyMatch!.source),
              subtitle: Text(
                'Match ${_readyMatch!.matchId}'
                '${_readyMatch!.seat == null ? '' : '\nSeat ${_readyMatch!.seat}'}\n'
                'Board handoff arrives in M3; live seat connection arrives in M4.',
              ),
              isThreeLine: true,
            ),
          ),
        ],
        const SizedBox(height: 20),
        Row(
          children: [
            Expanded(
              child: Text(
                'Rooms',
                style: Theme.of(context).textTheme.titleLarge,
              ),
            ),
            Text('${_matches.length} total'),
          ],
        ),
        const SizedBox(height: 8),
        if (_matches.isEmpty)
          const Card(
            child: ListTile(
              leading: Icon(Icons.inbox_outlined),
              title: Text('No rooms yet'),
              subtitle: Text('Create one or use matchmaking.'),
            ),
          )
        else
          ..._matches.map(
            (match) => Card(
              key: ValueKey('match-${match.id}'),
              child: ListTile(
                title: Text(match.id),
                subtitle: Text(
                  '${match.status} · ${match.players}/${match.capacity} seats · '
                  '${match.sequencesToWin} sequence${match.sequencesToWin == 1 ? '' : 's'} to win',
                ),
                trailing: TextButton(
                  key: ValueKey('open-${match.id}'),
                  onPressed: () => _chooseMatch(match),
                  child: const Text('Open'),
                ),
              ),
            ),
          ),
      ],
    );
  }
}

String _errorMessage(Object error) => switch (error) {
  TesseraApiException() => error.message,
  _ => 'Unexpected lobby error: $error',
};
