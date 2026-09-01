import 'package:flutter/material.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:http/http.dart' as http;

import 'health_screen.dart';
import 'identity_store.dart';
import 'lobby_screen.dart';

/// Mobile entry point: verify the configured backend, then enter the M2 lobby.
///
/// The [http.Client] is created once here (not per build) and handed to the
/// screen; it lives for the lifetime of the app, so it is never closed.
void main() {
  WidgetsFlutterBinding.ensureInitialized();
  final client = http.Client();
  const secureStorage = FlutterSecureStorage(
    aOptions: AndroidOptions(migrateWithBackup: true),
  );
  runApp(
    TesseraApp(
      httpClient: client,
      credentialStore: const SecureCredentialStore(secureStorage),
    ),
  );
}

class TesseraApp extends StatelessWidget {
  final http.Client httpClient;
  final CredentialStore credentialStore;

  const TesseraApp({
    super.key,
    required this.httpClient,
    required this.credentialStore,
  });

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Tessera',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Colors.indigo),
        useMaterial3: true,
      ),
      home: Builder(
        builder: (context) => HealthScreen(
          httpClient: httpClient,
          onContinue: (baseUrl) {
            Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => LobbyScreen(
                  httpClient: httpClient,
                  credentialStore: credentialStore,
                  baseUrl: baseUrl,
                ),
              ),
            );
          },
        ),
      ),
    );
  }
}
