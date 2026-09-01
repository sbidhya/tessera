import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'identity_store.dart';
import 'server_health.dart';
import 'tessera_api.dart';

/// M2 screen: identity + lobby (create / list / matchmake via REST).
///
/// Flow: point at a server → check it answers → mint an anonymous identity
/// (`POST /v1/players`, kept in the injected [IdentityStore]) → find a
/// partner through the matchmaking queue, create a match directly, or browse
/// the public match list. Board rendering and live play arrive in M3/M4; the
/// match this screen produces is shown as a match ID + seat to play from.
class LobbyScreen extends StatefulWidget {
  final http.Client httpClient;
  final IdentityStore identityStore;

  const LobbyScreen({
    super.key,
    required this.httpClient,
    required this.identityStore,
  });

  @override
  State<LobbyScreen> createState() => _LobbyScreenState();
}

enum _ServerState { unknown, checking, reachable, unreachable }

class _LobbyScreenState extends State<LobbyScreen> {
  late final TextEditingController _urlController;

  _ServerState _serverState = _ServerState.unknown;
  String? _serverDetail;

  bool _identityLoading = true;
  bool _identityCreating = false;
  PlayerIdentity? _identity;
  String? _identityError;

  int _sequencesToWin = 2;
  bool _searching = false;
  bool _cancelling = false;
  int _mmGeneration = 0; // stale long-poll completions are ignored
  String? _mmError;

  bool _creating = false;
  String? _createError;

  bool _listing = false;
  List<MatchSummary>? _matches;
  String? _listError;

  String? _activeMatchId;
  int? _activeSeat;

  TesseraApi get _api =>
      TesseraApi(client: widget.httpClient, baseUrl: _urlController.text);

  @override
  void initState() {
    super.initState();
    _urlController = TextEditingController(
      text: defaultBaseUrl(defaultTargetPlatform),
    );
    _checkServer();
    _loadIdentity();
  }

  @override
  void dispose() {
    _urlController.dispose();
    super.dispose();
  }

  Future<void> _checkServer() async {
    setState(() {
      _serverState = _ServerState.checking;
      _serverDetail = null;
    });
    try {
      final health = await fetchServerHealth(
        client: widget.httpClient,
        baseUrl: _urlController.text,
      );
      if (!mounted) return;
      setState(() {
        _serverState = _ServerState.reachable;
        _serverDetail = 'uptime ${health.uptime}';
      });
    } on ServerHealthException catch (e) {
      if (!mounted) return;
      setState(() {
        _serverState = _ServerState.unreachable;
        _serverDetail = e.message;
      });
    }
  }

  Future<void> _loadIdentity() async {
    final identity = await widget.identityStore.load();
    if (!mounted) return;
    setState(() {
      _identity = identity;
      _identityLoading = false;
    });
  }

  Future<void> _createIdentity() async {
    setState(() {
      _identityCreating = true;
      _identityError = null;
    });
    try {
      final identity = await _api.createPlayer();
      await widget.identityStore.save(identity);
      if (!mounted) return;
      setState(() => _identity = identity);
    } on TesseraApiException catch (e) {
      if (!mounted) return;
      setState(() {
        _identityError = e.code == 'auth_disabled'
            ? 'This server runs without the identity layer — it accepts any '
                  'player_id without a token (B3 legacy mode).'
            : e.message;
      });
    } finally {
      if (mounted) setState(() => _identityCreating = false);
    }
  }

  Future<void> _forgetIdentity() async {
    await widget.identityStore.clear();
    if (!mounted) return;
    setState(() {
      _identity = null;
      _activeMatchId = null;
      _activeSeat = null;
    });
  }

  /// Starts the matchmaking long-poll. Only one search runs at a time; a
  /// completion from an older generation (e.g. after cancel + re-search) is
  /// ignored so stale results can't overwrite fresh state.
  Future<void> _findMatch() async {
    final identity = _identity;
    if (identity == null || _searching) return;
    final generation = ++_mmGeneration;
    setState(() {
      _searching = true;
      _mmError = null;
      _activeMatchId = null;
      _activeSeat = null;
    });
    try {
      // The client's own queue budget (docs/match.md recommends ~30s, then
      // retry — the retry attaches to the existing queue entry server-side).
      final paired = await _api.joinMatchmaking(
        identity: identity,
        sequencesToWin: _sequencesToWin,
      );
      if (!mounted || generation != _mmGeneration) return;
      setState(() {
        if (paired == null) {
          _mmError = 'Withdrawn from the queue — no match was made.';
        } else {
          _activeMatchId = paired.matchId;
          _activeSeat = paired.seat;
        }
      });
    } on TesseraApiException catch (e) {
      if (!mounted || generation != _mmGeneration) return;
      setState(() {
        _mmError = e.code == 'timeout'
            ? 'Still waiting after 30s — you are still queued. Tap “Find match” again to keep waiting.'
            : e.message;
      });
    } finally {
      if (mounted && generation == _mmGeneration) {
        setState(() => _searching = false);
      }
    }
  }

  Future<void> _cancelSearch() async {
    final identity = _identity;
    if (identity == null || !_searching || _cancelling) return;
    setState(() {
      _cancelling = true;
      _mmError = null;
    });
    try {
      await _api.leaveMatchmaking(identity: identity);
      // Invalidate the in-flight long-poll: when the server ends it with
      // 204, the stale completion is ignored by the generation check.
      _mmGeneration++;
      if (!mounted) return;
      setState(() {
        _searching = false;
        _mmError = 'Left the queue.';
      });
    } on TesseraApiException catch (e) {
      if (!mounted) return;
      setState(() => _mmError = e.message);
    } finally {
      if (mounted) setState(() => _cancelling = false);
    }
  }

  Future<void> _createMatch() async {
    final identity = _identity;
    if (identity == null || _creating) return;
    setState(() {
      _creating = true;
      _createError = null;
      _activeMatchId = null;
      _activeSeat = null;
    });
    try {
      final match = await _api.createMatch(
        identity: identity,
        sequencesToWin: _sequencesToWin,
      );
      if (!mounted) return;
      setState(() {
        _activeMatchId = match.id;
        _activeSeat = null; // direct create doesn't seat the creator (M4 will)
      });
    } on TesseraApiException catch (e) {
      if (!mounted) return;
      setState(() => _createError = e.message);
    } finally {
      if (mounted) setState(() => _creating = false);
    }
  }

  Future<void> _refreshMatches() async {
    if (_listing) return;
    setState(() {
      _listing = true;
      _listError = null;
    });
    try {
      final matches = await _api.listMatches();
      if (!mounted) return;
      setState(() => _matches = matches);
    } on TesseraApiException catch (e) {
      if (!mounted) return;
      setState(() => _listError = e.message);
    } finally {
      if (mounted) setState(() => _listing = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Tessera — Lobby')),
      body: SingleChildScrollView(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _buildServerCard(),
            const SizedBox(height: 12),
            _buildIdentityCard(),
            const SizedBox(height: 12),
            _buildLobbyCard(),
            const SizedBox(height: 12),
            _buildMatchesCard(),
            if (_activeMatchId != null) ...[
              const SizedBox(height: 12),
              _buildActiveMatchCard(),
            ],
          ],
        ),
      ),
    );
  }

  Widget _buildServerCard() {
    final (icon, label) = switch (_serverState) {
      _ServerState.unknown => (Icons.help_outline, 'Not checked yet'),
      _ServerState.checking => (Icons.sync, 'Checking…'),
      _ServerState.reachable => (Icons.check_circle, 'Server reachable'),
      _ServerState.unreachable => (Icons.error, 'Server unreachable'),
    };
    final color = switch (_serverState) {
      _ServerState.reachable => Colors.green.shade700,
      _ServerState.unreachable => Colors.red.shade700,
      _ => null,
    };
    return Card(
      key: const Key('serverCard'),
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            TextField(
              key: const Key('baseUrlField'),
              controller: _urlController,
              keyboardType: TextInputType.url,
              autocorrect: false,
              decoration: const InputDecoration(
                labelText: 'Server base URL',
                hintText: 'http://localhost:8080',
                border: OutlineInputBorder(),
              ),
              onSubmitted: (_) {
                _checkServer();
              },
            ),
            const SizedBox(height: 8),
            Row(
              children: [
                Icon(icon, color: color, size: 20),
                const SizedBox(width: 8),
                Expanded(
                  child: Text(
                    _serverDetail == null ? label : '$label — $_serverDetail',
                    key: const Key('serverStatus'),
                  ),
                ),
                TextButton(
                  key: const Key('recheckButton'),
                  onPressed: _serverState == _ServerState.checking
                      ? null
                      : _checkServer,
                  child: const Text('Recheck'),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildIdentityCard() {
    Widget content;
    if (_identityLoading) {
      content = const ListTile(
        leading: SizedBox(
          width: 24,
          height: 24,
          child: CircularProgressIndicator(strokeWidth: 2),
        ),
        title: Text('Loading identity…'),
      );
    } else if (_identity case final identity?) {
      content = ListTile(
        leading: Icon(Icons.person, color: Colors.indigo.shade700),
        title: Text(
          'Player ${shortId(identity.playerId)}',
          key: const Key('playerId'),
        ),
        subtitle: const Text('Anonymous identity — the token proves this id.'),
        trailing: TextButton(
          key: const Key('forgetIdentityButton'),
          onPressed: _forgetIdentity,
          child: const Text('Forget'),
        ),
      );
    } else {
      content = Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          const ListTile(
            leading: Icon(Icons.person_outline),
            title: Text('No player identity yet'),
            subtitle: Text(
              'Mint an anonymous id to join matchmaking or create a match.',
            ),
          ),
          if (_identityError != null)
            Padding(
              padding: const EdgeInsets.symmetric(horizontal: 16),
              child: Text(
                _identityError!,
                key: const Key('identityError'),
                style: TextStyle(color: Colors.red.shade700),
              ),
            ),
          Padding(
            padding: const EdgeInsets.all(12),
            child: FilledButton.icon(
              key: const Key('createIdentityButton'),
              onPressed: _identityCreating ? null : _createIdentity,
              icon: const Icon(Icons.badge),
              label: Text(
                _identityCreating ? 'Creating…' : 'Create player identity',
              ),
            ),
          ),
        ],
      );
    }
    return Card(key: const Key('identityCard'), child: content);
  }

  Widget _buildLobbyCard() {
    final hasIdentity = _identity != null;
    return Card(
      key: const Key('lobbyCard'),
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                const Text('Sequences to win:'),
                const SizedBox(width: 12),
                SegmentedButton<int>(
                  key: const Key('sequencesSegment'),
                  segments: const [
                    ButtonSegment(value: 1, label: Text('1 (quick)')),
                    ButtonSegment(value: 2, label: Text('2 (standard)')),
                  ],
                  selected: {_sequencesToWin},
                  onSelectionChanged: hasIdentity && !_searching
                      ? (selected) =>
                            setState(() => _sequencesToWin = selected.first)
                      : null,
                ),
              ],
            ),
            const SizedBox(height: 8),
            if (_searching)
              FilledButton.icon(
                key: const Key('cancelSearchButton'),
                onPressed: _cancelling ? null : _cancelSearch,
                icon: const Icon(Icons.close),
                label: Text(_cancelling ? 'Leaving…' : 'Cancel search'),
              )
            else
              FilledButton.icon(
                key: const Key('findMatchButton'),
                onPressed: hasIdentity ? _findMatch : null,
                icon: const Icon(Icons.group_add),
                label: const Text('Find match (matchmaking)'),
              ),
            if (_searching)
              const Padding(
                padding: EdgeInsets.only(top: 8),
                child: Row(
                  children: [
                    SizedBox(
                      width: 16,
                      height: 16,
                      child: CircularProgressIndicator(strokeWidth: 2),
                    ),
                    SizedBox(width: 8),
                    Text('Waiting for a partner…', key: Key('searchingLabel')),
                  ],
                ),
              ),
            if (_mmError != null)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  _mmError!,
                  key: const Key('matchmakingError'),
                  style: TextStyle(color: Colors.red.shade700),
                ),
              ),
            const Divider(height: 24),
            OutlinedButton.icon(
              key: const Key('createMatchButton'),
              onPressed: hasIdentity && !_creating ? _createMatch : null,
              icon: const Icon(Icons.add),
              label: Text(_creating ? 'Creating…' : 'Create match directly'),
            ),
            if (_createError != null)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  _createError!,
                  key: const Key('createMatchError'),
                  style: TextStyle(color: Colors.red.shade700),
                ),
              ),
            if (!hasIdentity)
              const Padding(
                padding: EdgeInsets.only(top: 8),
                child: Text(
                  'Create a player identity above to use the lobby.',
                  key: Key('lobbyHint'),
                ),
              ),
          ],
        ),
      ),
    );
  }

  Widget _buildMatchesCard() {
    return Card(
      key: const Key('matchesCard'),
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                const Expanded(
                  child: Text(
                    'Open matches',
                    style: TextStyle(fontSize: 16, fontWeight: FontWeight.bold),
                  ),
                ),
                TextButton.icon(
                  key: const Key('refreshMatchesButton'),
                  onPressed: _listing ? null : _refreshMatches,
                  icon: const Icon(Icons.refresh, size: 18),
                  label: Text(_listing ? 'Loading…' : 'Refresh'),
                ),
              ],
            ),
            if (_listError != null)
              Text(
                _listError!,
                key: const Key('matchesError'),
                style: TextStyle(color: Colors.red.shade700),
              ),
            if (_matches != null && _matches!.isEmpty)
              const Text(
                'No matches yet — create one or wait for matchmaking.',
                key: Key('emptyMatches'),
              ),
            for (final match in _matches ?? [])
              ListTile(
                dense: true,
                contentPadding: EdgeInsets.zero,
                leading: const Icon(Icons.sports_esports),
                title: Text(
                  'Match ${shortId(match.id)}',
                  key: Key('match-${match.id}'),
                ),
                subtitle: Text(
                  '${match.status} · ${match.players}/${match.capacity} players · '
                  'first to ${match.sequencesToWin}',
                ),
              ),
          ],
        ),
      ),
    );
  }

  Widget _buildActiveMatchCard() {
    final seat = _activeSeat;
    return Card(
      key: const Key('activeMatchCard'),
      color: Colors.indigo.shade50,
      child: ListTile(
        leading: Icon(Icons.check_circle, color: Colors.indigo.shade700),
        title: Text(
          'Match ${_activeMatchId!}',
          key: const Key('activeMatchId'),
        ),
        subtitle: Text(
          seat == null
              ? 'Created — share the id; open the board in M3/M4 to play.'
              : 'Paired! You are seat $seat — open the board in M3/M4 to play.',
        ),
        isThreeLine: true,
      ),
    );
  }
}

/// Short display form of an id: first 8 chars plus an ellipsis when longer.
///
/// Ids are unguessable tokens (`p_…`, `r_…`); the full value never needs to
/// be visible in the lobby, but it stays copyable from the active-match card.
@visibleForTesting
String shortId(String id) => id.length <= 8 ? id : '${id.substring(0, 8)}…';
