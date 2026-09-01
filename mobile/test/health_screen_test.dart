import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:tessera/health_screen.dart';

/// M1 widget tests: the status screen against a mock HTTP client.
///
/// The screen fires one check on launch; each test pumps the screen with a
/// canned client and asserts the card it renders.

Future<void> pumpScreen(WidgetTester tester, http.Client client) async {
  await tester.pumpWidget(
    MaterialApp(home: HealthScreen(httpClient: client)),
  );
  await tester.pumpAndSettle();
}

void main() {
  testWidgets('shows reachable state on healthy response', (tester) async {
    await pumpScreen(
      tester,
      MockClient(
        (request) async =>
            http.Response('{"status":"ok","uptime":"3.21s"}', 200),
      ),
    );

    expect(find.text('Server reachable'), findsOneWidget);
    expect(find.textContaining('status: ok'), findsOneWidget);
    expect(find.textContaining('uptime: 3.21s'), findsOneWidget);
  });

  testWidgets('shows error state when the server is down', (tester) async {
    await pumpScreen(
      tester,
      MockClient((request) async {
        throw http.ClientException('Connection refused');
      }),
    );

    expect(find.text('Server unreachable'), findsOneWidget);
    expect(find.textContaining('could not reach'), findsOneWidget);
  });

  testWidgets('shows error state on non-200', (tester) async {
    await pumpScreen(
      tester,
      MockClient((request) async => http.Response('bad', 503)),
    );

    expect(find.text('Server unreachable'), findsOneWidget);
    expect(find.textContaining('503'), findsOneWidget);
  });

  testWidgets('re-check uses the edited URL', (tester) async {
    final requested = <Uri>[];
    await pumpScreen(
      tester,
      MockClient((request) async {
        requested.add(request.url);
        return http.Response('{"status":"ok","uptime":"0s"}', 200);
      }),
    );
    requested.clear();

    await tester.enterText(
      find.byKey(const Key('baseUrlField')),
      'http://example.com:9999',
    );
    await tester.tap(find.byKey(const Key('checkButton')));
    await tester.pumpAndSettle();

    expect(requested, hasLength(1));
    expect(requested.single.host, 'example.com');
    expect(requested.single.port, 9999);
    expect(requested.single.path, '/healthz');
    expect(find.text('Server reachable'), findsOneWidget);
  });

  testWidgets('check button is disabled while a check is in flight', (
    tester,
  ) async {
    // The mock holds the launch check open until we release it, so the test
    // can observe the in-flight state and still end with no pending timers.
    final release = Completer<http.Response>();
    await tester.pumpWidget(
      MaterialApp(
        home: HealthScreen(
          httpClient: MockClient((request) => release.future),
        ),
      ),
    );
    await tester.pump(); // start the launch check, but don't settle it

    expect(find.text('Checking…'), findsOneWidget);
    final button = tester.widget<FilledButton>(
      find.byKey(const Key('checkButton')),
    );
    expect(button.onPressed, isNull);

    release.complete(http.Response('{"status":"ok","uptime":"0s"}', 200));
    await tester.pumpAndSettle();
    expect(find.text('Server reachable'), findsOneWidget);
  });
}
