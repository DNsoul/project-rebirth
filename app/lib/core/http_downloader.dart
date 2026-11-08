import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data' show BytesBuilder;

import 'name_resolver.dart';
import 'updater.dart';

/// Downloads over https, following redirects by hand: GitHub answers a release
/// download with a redirect to another host, and every step of it has to be
/// https too, which the client's own redirect handling does not insist on.
///
/// A file can be fetched three ways ([DownloadRoute]); which one is asked for is
/// up to the caller, which tries them in turn.
class HttpDownloader implements Downloader {
  HttpDownloader({
    HttpClient? client,
    NameResolver? resolver,
    Uri? apiBase,
    this.timeout = const Duration(seconds: 30),
    this.maxRedirects = 5,
    this.allowPlainHttp = false,
  }) : _client = client ?? HttpClient(),
       _resolver = resolver ?? DohResolver(),
       _apiBase = apiBase ?? Uri.parse('https://api.github.com');

  final Duration timeout;
  final int maxRedirects;

  /// For tests, which talk to a server on this machine. Never set in the apps.
  final bool allowPlainHttp;
  final HttpClient _client;
  final NameResolver _resolver;

  /// Where GitHub's API is.
  final Uri _apiBase;

  /// The client of the route that goes round the tunnel, made when first needed.
  HttpClient? _around;

  static const _redirects = {
    HttpStatus.movedPermanently,
    HttpStatus.found,
    HttpStatus.seeOther,
    HttpStatus.temporaryRedirect,
    HttpStatus.permanentRedirect,
  };

  /// `https://github.com/<owner>/<repo>/releases/download/<tag>/<file>`
  static final _githubDownload = RegExp(
    r'^/([^/]+)/([^/]+)/releases/download/([^/]+)/([^/]+)$',
  );

  @override
  Future<void> download(
    Uri url,
    File file, {
    required int limit,
    void Function(int received)? onProgress,
    DownloadRoute route = DownloadRoute.usual,
  }) async {
    try {
      switch (route) {
        case DownloadRoute.usual:
          await _fetch(_client, url, file, limit, onProgress);
        case DownloadRoute.api:
          final address = await _apiAddressOf(url);
          await _fetch(
            _client,
            address,
            file,
            limit,
            onProgress,
            accept: 'application/octet-stream',
          );
        case DownloadRoute.direct:
          await _fetch(_roundTheTunnel(), url, file, limit, onProgress);
      }
    } on UpdateException {
      rethrow;
    } on TimeoutException {
      throw UpdateException('Сервер обновления не отвечает. Повторите позже.');
    } on SocketException {
      throw UpdateException(
        'Нет связи с сервером обновления. Проверьте интернет.',
      );
    } on HandshakeException {
      throw UpdateException(
        'Не удалось установить защищённое соединение. Проверьте дату и время на устройстве.',
      );
    } on HttpException catch (e) {
      throw UpdateException(
        'Не удалось скачать обновление. Повторите позже. (${e.message})',
      );
    }
  }

  @override
  bool canUse(DownloadRoute route, Uri url) =>
      route != DownloadRoute.api || _isGithubRelease(url);

  static bool _isGithubRelease(Uri url) =>
      url.host == 'github.com' && _githubDownload.hasMatch(url.path);

  void _requireSecure(Uri at) {
    if (at.scheme != 'https' && !(allowPlainHttp && at.scheme == 'http')) {
      throw UpdateException(
        'Адрес для скачивания не защищён (${at.scheme}://). Отказываюсь качать.',
      );
    }
  }

  /// The file at [url], with the redirects it leads through, into [file].
  Future<void> _fetch(
    HttpClient client,
    Uri url,
    File file,
    int limit,
    void Function(int received)? onProgress, {
    String? accept,
  }) async {
    var at = url;
    for (var hop = 0; hop <= maxRedirects; hop++) {
      _requireSecure(at);
      final request = await client.getUrl(at).timeout(timeout);
      request.followRedirects = false;
      request.headers.set(HttpHeaders.userAgentHeader, 'rebirth-updater');
      if (accept != null) {
        request.headers.set(HttpHeaders.acceptHeader, accept);
      }
      final response = await request.close().timeout(timeout);

      if (_redirects.contains(response.statusCode)) {
        final to = response.headers.value(HttpHeaders.locationHeader);
        await response.drain<void>();
        if (to == null) {
          throw UpdateException('Сервер перенаправил, не сказав куда.');
        }
        at = at.resolve(to);
        continue;
      }
      if (response.statusCode != HttpStatus.ok) {
        await response.drain<void>();
        throw UpdateException(_said(response.statusCode));
      }
      await _save(response, file, limit, onProgress);
      return;
    }
    throw UpdateException(
      'Слишком много перенаправлений при скачивании. Повторите позже.',
    );
  }

  /// What to tell a person about an answer that is not the file. A server that
  /// has trouble is not the same as a file that is not there.
  static String _said(int status) {
    if (status >= 500) {
      return 'Сервер обновления сейчас не отвечает (ответ сервера $status). Повторите позже.';
    }
    if (status == HttpStatus.notFound) {
      return 'Файл обновления не найден (ответ сервера $status). Повторите позже.';
    }
    return 'Сервер не отдал файл обновления (ответ сервера $status). Повторите позже.';
  }

  // ------------------------------------------------------------- the API route

  /// GitHub's API address of the file that a release link names. The link is
  /// served by one of GitHub's front-ends, the API by another; the same file is
  /// behind both.
  Future<Uri> _apiAddressOf(Uri url) async {
    final m = _githubDownload.firstMatch(url.path);
    if (url.host != 'github.com' || m == null) {
      throw UpdateException('Для этого адреса нет запасного пути.');
    }
    final tag = Uri.decodeComponent(m[3]!);
    final name = Uri.decodeComponent(m[4]!);
    final release = await _getJson(
      _apiBase.replace(
        path:
            '/repos/${m[1]}/${m[2]}/releases/tags/${Uri.encodeComponent(tag)}',
      ),
    );
    final assets = release is Map ? release['assets'] : null;
    if (assets is List) {
      for (final a in assets) {
        if (a is Map && a['name'] == name && a['url'] is String) {
          return Uri.parse(a['url'] as String);
        }
      }
    }
    throw UpdateException('Файла $name нет среди объявленных на GitHub.');
  }

  Future<Object?> _getJson(Uri at) async {
    _requireSecure(at);
    final request = await _client.getUrl(at).timeout(timeout);
    request.headers
      ..set(HttpHeaders.userAgentHeader, 'rebirth-updater')
      ..set(HttpHeaders.acceptHeader, 'application/vnd.github+json');
    final response = await request.close().timeout(timeout);
    if (response.statusCode != HttpStatus.ok) {
      await response.drain<void>();
      throw UpdateException(_said(response.statusCode));
    }
    final body = BytesBuilder();
    await for (final chunk in response.timeout(timeout)) {
      body.add(chunk);
      if (body.length > 1024 * 1024) {
        throw UpdateException('Слишком большой ответ сервера обновления.');
      }
    }
    try {
      return jsonDecode(utf8.decode(body.takeBytes(), allowMalformed: true));
    } on FormatException {
      throw UpdateException('Сервер обновления ответил непонятно.');
    }
  }

  // --------------------------------------------------- the route round the tunnel

  /// A client that finds a name's address itself, outside the machine's resolver,
  /// and connects to it straight. The certificate is still checked against the
  /// name, so reaching the address another way gives nobody anything.
  HttpClient _roundTheTunnel() {
    var client = _around;
    if (client == null) {
      client = HttpClient();
      // No proxy of the system's: the route is straight by its very name.
      client.findProxy = (_) => 'DIRECT';
      client.connectionFactory = _connectStraight;
      _around = client;
    }
    return client;
  }

  Future<ConnectionTask<Socket>> _connectStraight(
    Uri uri,
    String? proxyHost,
    int? proxyPort,
  ) async {
    final addresses = await _resolver.resolve(uri.host);
    Object? last;
    // One address that does not answer is followed by the next. Each gets a part
    // of the time, not all of it, or the first that stays silent would use it up.
    final each = Duration(milliseconds: timeout.inMilliseconds ~/ 3);
    for (final address in addresses) {
      try {
        final tcp = await Socket.connect(address, uri.port, timeout: each);
        if (uri.scheme != 'https') {
          return ConnectionTask.fromSocket(Future.value(tcp), tcp.destroy);
        }
        // The handshake is made here: the client does not make it for a
        // connection it did not open itself.
        return ConnectionTask.fromSocket(
          SecureSocket.secure(tcp, host: uri.host),
          tcp.destroy,
        );
      } on SocketException catch (e) {
        last = e;
      }
    }
    throw last ?? SocketException('Нет адреса для ${uri.host}');
  }

  Future<void> _save(
    Stream<List<int>> body,
    File file,
    int limit,
    void Function(int received)? onProgress,
  ) async {
    final sink = file.openWrite();
    var received = 0;
    try {
      // The timeout is for silence between pieces, not for the whole file.
      await for (final chunk in body.timeout(timeout)) {
        received += chunk.length;
        if (received > limit) {
          throw UpdateException(
            'Сервер отдаёт больше, чем объявлено ($limit байт). Скачивание остановлено, повторите позже.',
          );
        }
        sink.add(chunk);
        onProgress?.call(received);
      }
      await sink.flush();
    } finally {
      await sink.close();
    }
  }
}
