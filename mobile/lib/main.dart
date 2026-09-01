import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'health_screen.dart';

/// M1 entry point: a single screen proving the app can reach the backend.
///
/// The [http.Client] is created once here (not per build) and handed to the
/// screen; it lives for the lifetime of the app, so it is never closed.
void main() {
  final client = http.Client();
  runApp(TesseraApp(httpClient: client));
}

class TesseraApp extends StatelessWidget {
  final http.Client httpClient;

  const TesseraApp({super.key, required this.httpClient});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Tessera',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Colors.indigo),
        useMaterial3: true,
      ),
      home: HealthScreen(httpClient: httpClient),
    );
  }
}
