/// Normalizes a user-entered Tessera server URL to its HTTP(S) origin.
///
/// The backend routes are rooted at `/`, so any path, query, or fragment in
/// the entered value is deliberately discarded. A missing scheme means HTTP,
/// which keeps local emulator development convenient.
String normalizeServerUrl(String input) {
  var value = input.trim();
  if (value.isEmpty) {
    throw const FormatException('server URL is empty');
  }
  if (!value.contains('://')) {
    value = 'http://$value';
  }

  final uri = Uri.parse(value);
  if ((uri.scheme != 'http' && uri.scheme != 'https') || uri.host.isEmpty) {
    throw FormatException('server URL must be an HTTP(S) origin: $input');
  }
  return uri.origin;
}

/// Resolves one backend route against a normalized server origin.
Uri serverEndpoint(String baseUrl, String path) {
  final origin = Uri.parse(normalizeServerUrl(baseUrl));
  return origin.resolve(path.startsWith('/') ? path : '/$path');
}
