import 'dart:io';

const _configuredBackendUrl = String.fromEnvironment('TESSERA_BACKEND_URL');

/// Returns the backend URL for this build.
///
/// Android emulators reach the host machine through `10.0.2.2`; iOS
/// simulators can use loopback. A physical device needs a URL reachable from
/// that device, supplied with `--dart-define=TESSERA_BACKEND_URL=...`.
Uri backendBaseUri({
  bool? isAndroid,
  String configuredUrl = _configuredBackendUrl,
}) {
  if (configuredUrl.isNotEmpty) {
    final uri = Uri.parse(configuredUrl);
    if (!uri.hasScheme ||
        !uri.hasAuthority ||
        (uri.scheme != 'http' && uri.scheme != 'https')) {
      throw FormatException(
        'TESSERA_BACKEND_URL must be an absolute HTTP(S) URL',
        configuredUrl,
      );
    }
    return uri;
  }

  final host = (isAndroid ?? Platform.isAndroid) ? '10.0.2.2' : '127.0.0.1';
  return Uri(scheme: 'http', host: host, port: 8080);
}
