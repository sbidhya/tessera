import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'server_health.dart';

/// Default server origin per platform.
///
/// `localhost` on a phone points at the phone itself, not the dev machine:
/// the Android emulator aliases the host loopback as `10.0.2.2`, while the
/// iOS simulator shares the Mac's network so plain `localhost` works.
/// The field is editable regardless — this is just the starting guess.
String defaultBaseUrl(TargetPlatform platform) =>
    platform == TargetPlatform.android ? 'http://10.0.2.2:8080' : 'http://localhost:8080';

/// M1 screen: prove the app can reach the backend's `GET /healthz`.
///
/// Shows the reported status/uptime on success and the failure reason
/// otherwise. Later blocks (M2+) will reuse [fetchServerHealth] as the
/// connectivity probe underneath the lobby.
class HealthScreen extends StatefulWidget {
  /// HTTP client used for health checks. Injected so tests can pass a mock;
  /// production passes a real `http.Client` from `main()`.
  final http.Client httpClient;

  const HealthScreen({super.key, required this.httpClient});

  @override
  State<HealthScreen> createState() => _HealthScreenState();
}

enum _CheckState { idle, checking, ok, error }

class _HealthScreenState extends State<HealthScreen> {
  late final TextEditingController _urlController;
  _CheckState _state = _CheckState.idle;
  ServerHealth? _health;
  String? _error;

  @override
  void initState() {
    super.initState();
    _urlController = TextEditingController(
      text: defaultBaseUrl(defaultTargetPlatform),
    );
    // Run one check on launch so the gate ("show server status") is visible
    // without requiring a tap.
    _check();
  }

  @override
  void dispose() {
    _urlController.dispose();
    super.dispose();
  }

  Future<void> _check() async {
    setState(() {
      _state = _CheckState.checking;
      _error = null;
    });
    try {
      final health = await fetchServerHealth(
        client: widget.httpClient,
        baseUrl: _urlController.text,
      );
      if (!mounted) return;
      setState(() {
        _state = _CheckState.ok;
        _health = health;
      });
    } on ServerHealthException catch (e) {
      if (!mounted) return;
      setState(() {
        _state = _CheckState.error;
        _error = e.message;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Tessera — Server Status')),
      body: Padding(
        padding: const EdgeInsets.all(16),
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
              onSubmitted: (_) => _check(),
            ),
            const SizedBox(height: 12),
            FilledButton.icon(
              key: const Key('checkButton'),
              onPressed: _state == _CheckState.checking ? null : _check,
              icon: const Icon(Icons.refresh),
              label: const Text('Check server status'),
            ),
            const SizedBox(height: 24),
            _buildStatusCard(),
          ],
        ),
      ),
    );
  }

  Widget _buildStatusCard() {
    switch (_state) {
      case _CheckState.idle:
        return const Card(
          key: Key('statusCard'),
          child: ListTile(
            leading: Icon(Icons.help_outline),
            title: Text('Not checked yet'),
          ),
        );
      case _CheckState.checking:
        return const Card(
          key: Key('statusCard'),
          child: ListTile(
            leading: SizedBox(
              width: 24,
              height: 24,
              child: CircularProgressIndicator(strokeWidth: 2),
            ),
            title: Text('Checking…'),
          ),
        );
      case _CheckState.ok:
        final health = _health!;
        return Card(
          key: const Key('statusCard'),
          color: Colors.green.shade50,
          child: ListTile(
            leading: Icon(
              Icons.check_circle,
              color: Colors.green.shade700,
              size: 32,
            ),
            title: const Text('Server reachable'),
            subtitle: Text(
              'status: ${health.status}\n'
              'uptime: ${health.uptime}',
            ),
            isThreeLine: true,
          ),
        );
      case _CheckState.error:
        return Card(
          key: const Key('statusCard'),
          color: Colors.red.shade50,
          child: ListTile(
            leading: Icon(Icons.error, color: Colors.red.shade700, size: 32),
            title: const Text('Server unreachable'),
            subtitle: Text(_error ?? 'unknown error'),
            isThreeLine: true,
          ),
        );
    }
  }
}
