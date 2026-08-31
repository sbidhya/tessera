import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

import 'backend_config.dart';
import 'health_api.dart';
import 'health_screen.dart';

void main() {
  runApp(
    TesseraApp(
      healthSource: HealthApi(client: http.Client(), baseUri: backendBaseUri()),
    ),
  );
}

class TesseraApp extends StatelessWidget {
  const TesseraApp({required this.healthSource, super.key});

  final HealthSource healthSource;

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Tessera',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: const Color(0xFF245C46),
          brightness: Brightness.light,
        ),
        scaffoldBackgroundColor: const Color(0xFFF5F2E9),
        useMaterial3: true,
      ),
      home: HealthScreen(healthSource: healthSource),
    );
  }
}
