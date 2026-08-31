import 'package:flutter/material.dart';

import 'health_api.dart';

class HealthScreen extends StatefulWidget {
  const HealthScreen({required this.healthSource, super.key});

  final HealthSource healthSource;

  @override
  State<HealthScreen> createState() => _HealthScreenState();
}

class _HealthScreenState extends State<HealthScreen> {
  ServerHealth? _health;
  Object? _error;
  var _requestNumber = 0;

  bool get _isLoading => _health == null && _error == null;

  @override
  void initState() {
    super.initState();
    _loadHealth();
  }

  @override
  void didUpdateWidget(HealthScreen oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (widget.healthSource != oldWidget.healthSource) {
      _loadHealth();
    }
  }

  Future<void> _loadHealth() async {
    final requestNumber = ++_requestNumber;
    setState(() {
      _health = null;
      _error = null;
    });

    try {
      final health = await widget.healthSource.fetchHealth();
      if (!mounted || requestNumber != _requestNumber) return;
      setState(() => _health = health);
    } on Object catch (error) {
      if (!mounted || requestNumber != _requestNumber) return;
      setState(() => _error = error);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Tessera')),
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(24),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 440),
              child: Card(
                child: Padding(
                  padding: const EdgeInsets.all(32),
                  child: AnimatedSwitcher(
                    duration: const Duration(milliseconds: 200),
                    child: _buildStatus(context),
                  ),
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }

  Widget _buildStatus(BuildContext context) {
    if (_isLoading) {
      return const Column(
        key: ValueKey('loading'),
        mainAxisSize: MainAxisSize.min,
        children: [
          CircularProgressIndicator(),
          SizedBox(height: 20),
          Text('Connecting to Tessera…'),
        ],
      );
    }

    if (_error != null) {
      return Column(
        key: const ValueKey('error'),
        mainAxisSize: MainAxisSize.min,
        children: [
          Icon(
            Icons.cloud_off_outlined,
            color: Theme.of(context).colorScheme.error,
            size: 48,
          ),
          const SizedBox(height: 16),
          Text(
            'Server unavailable',
            style: Theme.of(context).textTheme.headlineSmall,
          ),
          const SizedBox(height: 8),
          const Text(
            'Could not reach the Tessera backend. Check that it is running and try again.',
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 20),
          FilledButton.icon(
            onPressed: _loadHealth,
            icon: const Icon(Icons.refresh),
            label: const Text('Retry'),
          ),
        ],
      );
    }

    final health = _health!;
    return Column(
      key: const ValueKey('healthy'),
      mainAxisSize: MainAxisSize.min,
      children: [
        Icon(
          Icons.check_circle_outline,
          color: Theme.of(context).colorScheme.primary,
          size: 52,
        ),
        const SizedBox(height: 16),
        Text('Server online', style: Theme.of(context).textTheme.headlineSmall),
        const SizedBox(height: 12),
        Text('Status: ${health.status}'),
        Text('Uptime: ${health.uptime}'),
      ],
    );
  }
}
