/// Takes out of a log line what must not leave the device in a journal that a
/// person sends to someone: keys, tokens, the secret gist, and the addresses of
/// the servers.
///
/// What stays is what helps to find out what went wrong: the addresses inside
/// the tunnel and on the home network (10.x, 192.168.x and the like), which say
/// nothing about where the servers are, and the first and last part of a
/// server's address, which tell two servers apart without naming either.
class LogScrubber {
  LogScrubber({Iterable<String> secrets = const []}) {
    for (final s in secrets) {
      addSecret(s);
    }
  }

  final _secrets = <String>{};

  /// Something to take out wherever it appears, word for word: the gist's id,
  /// its owner, the key the configuration is decrypted with. Short strings are
  /// ignored: four letters would take half the words of the log with them.
  void addSecret(String secret) {
    final s = secret.trim();
    if (s.length >= 6) {
      _secrets.add(s);
    }
  }

  // A line of a configuration file that carries a key.
  static final _keyLine = RegExp(
    r'\b(PrivateKey|PresharedKey|PublicKey|private_key|preshared_key)(\s*[=:]\s*)\S+',
    caseSensitive: false,
  );

  // A key as WireGuard writes it: 32 bytes in base64.
  static final _wgKey = RegExp(r'(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{42,43}=');

  // GitHub's tokens, in case one ever ends up in an error message.
  static final _ghToken = RegExp(
    r'\b(gh[pousr]_|github_pat_)[A-Za-z0-9_]{16,}',
  );

  // Long runs of hex (the gist's id is 32) or of base64 with no dots in them:
  // hashes, ids and sealed blobs. Nothing a person reads is that long.
  static final _hex = RegExp(r'\b[0-9a-fA-F]{32,}\b');
  // (With both a digit and a letter in it: a row of dashes, or a long word,
  // is not one.)
  static final _blob = RegExp(
    r'(?<![A-Za-z0-9+/_-])(?=[A-Za-z+/_-]*\d)(?=[\d+/_-]*[A-Za-z])'
    r'[A-Za-z0-9+/_-]{40,}={0,2}',
  );

  // An address on the web: the host stays, the path (where the gist's id and
  // owner are) goes.
  static final _url = RegExp(r'''\b(https?)://([^\s/"'<>]+)(/[^\s"'<>]*)?''');

  static final _ipv4 = RegExp(
    r'(?<![\d.])(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.'
    r'(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?![\d.]*\d)',
  );

  // Full (eight groups) or shortened (with "::") IPv6 addresses. Clock times
  // (12:34:56) have neither, and are left alone.
  static final _ipv6 = RegExp(
    r'(?<![\w:.])('
    r'[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){7}'
    r'|(?:[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6})?::(?:[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6})?'
    r')(?![\w:])',
  );

  /// [line] with everything secret taken out.
  String scrub(String line) {
    var s = line;
    for (final secret in _secrets) {
      s = s.replaceAll(secret, '‹secret›');
    }
    s = s.replaceAllMapped(_keyLine, (m) => '${m[1]}${m[2]}‹hidden›');
    s = s.replaceAllMapped(_url, (m) {
      final host = maskHost(m[2]!);
      final path = m[3];
      return '${m[1]}://$host${path == null || path == '/' ? '' : '/…'}';
    });
    s = s.replaceAll(_ghToken, '‹token›');
    s = s.replaceAll(_wgKey, '‹key›');
    s = s.replaceAll(_hex, '‹hex›');
    s = s.replaceAll(_blob, '‹blob›');
    s = s.replaceAllMapped(_ipv4, (m) => maskIpv4(m[0]!));
    s = s.replaceAllMapped(_ipv6, (m) => maskIpv6(m[0]!));
    return s;
  }

  /// A host name stays; an address as a host is masked like any other.
  static String maskHost(String host) {
    final v4 = _ipv4.firstMatch(host);
    if (v4 != null) {
      return host.replaceRange(v4.start, v4.end, maskIpv4(v4[0]!));
    }
    return host;
  }

  /// `203.0.113.45` → `203.x.x.45`; private and special addresses are kept.
  static String maskIpv4(String ip) {
    final p = ip.split('.').map(int.parse).toList();
    final private =
        p[0] == 10 ||
        p[0] == 127 ||
        p[0] == 0 ||
        p[0] >= 224 ||
        (p[0] == 172 && p[1] >= 16 && p[1] <= 31) ||
        (p[0] == 192 && p[1] == 168) ||
        (p[0] == 169 && p[1] == 254) ||
        // The servers' resolver inside the tunnel is in 198.18.0.0/15, a range
        // that is never routed on the internet.
        (p[0] == 198 && (p[1] == 18 || p[1] == 19)) ||
        (p[0] == 100 && p[1] >= 64 && p[1] <= 127);
    if (private) return ip;
    return '${p[0]}.x.x.${p[3]}';
  }

  /// `2a01:4f8:c0c:1234::1` → `2a01:…:1`; link-local, unique-local, loopback
  /// and the unspecified address are kept.
  static String maskIpv6(String ip) {
    final lower = ip.toLowerCase();
    final groups = lower.split(':').where((g) => g.isNotEmpty).toList();
    if (groups.isEmpty || lower == '::1') return ip;
    final first = int.tryParse(groups.first, radix: 16) ?? 0;
    final leadingZero = lower.startsWith('::');
    final private =
        leadingZero ||
        (first & 0xffc0) == 0xfe80 || // link-local
        (first & 0xfe00) == 0xfc00; // unique-local
    if (private) return ip;
    if (groups.length == 1) return '${groups.first}:…';
    return '${groups.first}:…:${groups.last}';
  }
}
