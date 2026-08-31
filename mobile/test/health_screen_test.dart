import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:tessera/health_api.dart';
import 'package:tessera/main.dart';

void main() {
  testWidgets('shows progress followed by the server status', (tester) async {
    final result = Completer<ServerHealth>();
    await tester.pumpWidget(
      TesseraApp(healthSource: FakeHealthSource(() => result.future)),
    );

    expect(find.byType(CircularProgressIndicator), findsOneWidget);
    expect(find.text('Connecting to Tessera…'), findsOneWidget);

    result.complete(const ServerHealth(status: 'ok', uptime: '2s'));
    await tester.pumpAndSettle();

    expect(find.text('Server online'), findsOneWidget);
    expect(find.text('Status: ok'), findsOneWidget);
    expect(find.text('Uptime: 2s'), findsOneWidget);
  });

  testWidgets('shows an error and retries the request', (tester) async {
    var attempt = 0;
    final source = FakeHealthSource(() async {
      attempt++;
      if (attempt == 1) throw const HealthException('offline');
      return const ServerHealth(status: 'ok', uptime: '3s');
    });

    await tester.pumpWidget(TesseraApp(healthSource: source));
    await tester.pumpAndSettle();

    expect(find.text('Server unavailable'), findsOneWidget);
    expect(find.text('Retry'), findsOneWidget);

    await tester.tap(find.text('Retry'));
    await tester.pumpAndSettle();

    expect(attempt, 2);
    expect(find.text('Server online'), findsOneWidget);
    expect(find.text('Uptime: 3s'), findsOneWidget);
  });
}

class FakeHealthSource implements HealthSource {
  FakeHealthSource(this._fetch);

  final Future<ServerHealth> Function() _fetch;

  @override
  Future<ServerHealth> fetchHealth() => _fetch();
}
