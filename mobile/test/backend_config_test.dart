import 'package:flutter_test/flutter_test.dart';
import 'package:tessera/backend_config.dart';

void main() {
  test('uses the Android emulator host by default', () {
    expect(
      backendBaseUri(isAndroid: true, configuredUrl: '').toString(),
      'http://10.0.2.2:8080',
    );
  });

  test('uses loopback for the iOS simulator by default', () {
    expect(
      backendBaseUri(isAndroid: false, configuredUrl: '').toString(),
      'http://127.0.0.1:8080',
    );
  });

  test('uses a valid configured URL', () {
    expect(
      backendBaseUri(
        isAndroid: true,
        configuredUrl: 'https://api.example.test:8443/base',
      ).toString(),
      'https://api.example.test:8443/base',
    );
  });

  test('rejects a configured URL without an HTTP(S) authority', () {
    expect(
      () => backendBaseUri(configuredUrl: 'localhost:8080'),
      throwsFormatException,
    );
  });
}
