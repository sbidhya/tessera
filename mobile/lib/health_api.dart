import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

class ServerHealth {
  const ServerHealth({required this.status, required this.uptime});

  final String status;
  final String uptime;
}

abstract interface class HealthSource {
  Future<ServerHealth> fetchHealth();
}

class HealthApi implements HealthSource {
  HealthApi({
    required this.client,
    required Uri baseUri,
    this.timeout = const Duration(seconds: 5),
  }) : _healthUri = baseUri.resolve('/healthz');

  final http.Client client;
  final Uri _healthUri;
  final Duration timeout;

  @override
  Future<ServerHealth> fetchHealth() async {
    final response = await client.get(_healthUri).timeout(timeout);
    if (response.statusCode != 200) {
      throw HealthException('server returned HTTP ${response.statusCode}');
    }

    final Object? decoded;
    try {
      decoded = jsonDecode(response.body);
    } on FormatException {
      throw const HealthException('server returned invalid JSON');
    }

    if (decoded is! Map<String, dynamic> ||
        decoded['status'] is! String ||
        decoded['uptime'] is! String) {
      throw const HealthException('server returned an invalid health response');
    }

    return ServerHealth(
      status: decoded['status'] as String,
      uptime: decoded['uptime'] as String,
    );
  }
}

class HealthException implements Exception {
  const HealthException(this.message);

  final String message;

  @override
  String toString() => message;
}
