import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data' show BytesBuilder;

/// Finds the addresses of a name.
abstract class NameResolver {
  /// The addresses of [host], best first. Throws [SocketException] if there are
  /// none.
  Future<List<InternetAddress>> resolve(String host);
}

/// Asks public resolvers over https, by their own addresses (1.1.1.1 and
/// 8.8.8.8), so that the answer does not come from the machine's own resolver.
///
/// That matters here because the machine's resolver is not a neutral one: while
/// the tunnel is up, the names the server wants to carry are answered with
/// stand-in addresses that lead into the tunnel. A download that is to go round
/// the tunnel needs the name's real address.
///
/// Answers of all the servers are put together, so that if one gives a
/// front-end that is having a bad moment, another may give a different one.
class DohResolver implements NameResolver {
  DohResolver({
    HttpClient? client,
    List<Uri>? servers,
    this.timeout = const Duration(seconds: 6),
  }) : servers = servers ?? defaultServers,
       _client = client ?? (HttpClient()..findProxy = (_) => 'DIRECT');

  static final defaultServers = [
    Uri.parse('https://1.1.1.1/dns-query'),
    Uri.parse('https://8.8.8.8/resolve'),
  ];

  final List<Uri> servers;
  final Duration timeout;
  final HttpClient _client;

  /// More than this many addresses are not worth trying one after another.
  static const _most = 8;

  @override
  Future<List<InternetAddress>> resolve(String host) async {
    final literal = InternetAddress.tryParse(host);
    if (literal != null) {
      return [literal];
    }
    final answers = await Future.wait(
      servers.map(
        (s) => _ask(s, host).catchError((Object _) => <InternetAddress>[]),
      ),
    );
    final seen = <String>{};
    final all = [
      for (final list in answers)
        for (final a in list)
          if (seen.add(a.address)) a,
    ];
    if (all.isEmpty) {
      throw SocketException('Не удалось узнать адрес $host');
    }
    return all.take(_most).toList();
  }

  Future<List<InternetAddress>> _ask(Uri server, String host) async {
    final request = await _client
        .getUrl(server.replace(queryParameters: {'name': host, 'type': 'A'}))
        .timeout(timeout);
    request.headers.set(HttpHeaders.acceptHeader, 'application/dns-json');
    final response = await request.close().timeout(timeout);
    if (response.statusCode != HttpStatus.ok) {
      await response.drain<void>();
      return const [];
    }
    final body = BytesBuilder();
    await for (final chunk in response.timeout(timeout)) {
      body.add(chunk);
      if (body.length > 64 * 1024) {
        return const [];
      }
    }
    final Object? json;
    try {
      json = jsonDecode(utf8.decode(body.takeBytes(), allowMalformed: true));
    } on FormatException {
      return const [];
    }
    final answer = json is Map ? json['Answer'] : null;
    if (answer is! List) {
      return const [];
    }
    final out = <InternetAddress>[];
    for (final a in answer) {
      // Type 1 is an address (A); the rest of the answer, such as the CNAMEs that
      // lead to it, is not.
      if (a is Map && a['type'] == 1) {
        final ip = InternetAddress.tryParse('${a['data']}');
        if (ip != null &&
            ip.type == InternetAddressType.IPv4 &&
            !ip.isLoopback &&
            !ip.isLinkLocal &&
            !ip.isMulticast) {
          out.add(ip);
        }
      }
    }
    return out;
  }
}
