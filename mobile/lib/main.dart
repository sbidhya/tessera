import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'identity_store.dart';
import 'lobby_screen.dart';

/// M2 entry point: the lobby (identity + matchmaking + match list).
///
/// The [http.Client] and [IdentityStore] are created once here (not per
/// build) and handed to the screen; both live for the lifetime of the app.
/// M1's health screen remains in the repo (and tested) as the connectivity
/// probe's original home; the lobby embeds the same `/healthz` check as a
/// compact status row.
void main() {
  final client = http.Client();
  final identityStore = InMemoryIdentityStore();
  runApp(TesseraApp(httpClient: client, identityStore: identityStore));
}

class TesseraApp extends StatelessWidget {
  final http.Client httpClient;
  final IdentityStore identityStore;

  const TesseraApp({
    super.key,
    required this.httpClient,
    required this.identityStore,
  });

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Tessera',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Colors.indigo),
        useMaterial3: true,
      ),
      home: LobbyScreen(httpClient: httpClient, identityStore: identityStore),
    );
  }
}
