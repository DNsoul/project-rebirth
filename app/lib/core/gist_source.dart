import 'dart:async';
import 'dart:convert';
import 'dart:io';

/// What was in the gist at the moment it was read.
class GistSnapshot {
  const GistSnapshot({this.config, this.revocation, this.release, this.etag});

  /// The sealed configuration (file `c`), or null if the gist has none.
  final String? config;

  /// The sealed revocation record (file `r`).
  final String? revocation;

  /// The sealed announcement of a new version (file `u`).
  final String? release;

  /// Identifies this version of the gist, so asking again can be answered with
  /// "no change" without spending a request.
  final String? etag;
}

class GistException implements Exception {
  GistException(this.message);

  final String message;

  @override
  String toString() => message;
}

abstract class GistSource {
  /// Reads the gist. Returns null when [etag] says nothing changed. Throws
  /// [GistException] when it could not be read.
  Future<GistSnapshot?> fetch({String? etag});
}

/// Reads a secret gist through GitHub's API, without an account: knowing the
/// gist's address is what grants access to a secret gist.
class GithubGistSource implements GistSource {
  GithubGistSource(
    this.gistId, {
    HttpClient? client,
    this.baseUrl = 'https://api.github.com',
    this.timeout = const Duration(seconds: 20),
  }) : _client = client ?? HttpClient();

  final String gistId;
  final String baseUrl;
  final Duration timeout;
  final HttpClient _client;

  @override
  Future<GistSnapshot?> fetch({String? etag}) async {
    final Map<String, Object?> json;
    final String? newEtag;
    try {
      final request = await _client
          .getUrl(Uri.parse('$baseUrl/gists/$gistId'))
          .timeout(timeout);
      request.headers
        ..set(HttpHeaders.acceptHeader, 'application/vnd.github+json')
        // GitHub refuses requests that do not say who is asking.
        ..set(HttpHeaders.userAgentHeader, 'awg-client');
      if (etag != null && etag.isNotEmpty) {
        request.headers.set(HttpHeaders.ifNoneMatchHeader, etag);
      }

      final response = await request.close().timeout(timeout);
      if (response.statusCode == HttpStatus.notModified) {
        await response.drain<void>();
        return null;
      }
      final text = await response
          .transform(utf8.decoder)
          .join()
          .timeout(timeout);
      if (response.statusCode == HttpStatus.forbidden ||
          response.statusCode == HttpStatus.tooManyRequests) {
        throw GistException(
          'GitHub временно ограничил запросы (лимит). Повторите через несколько минут.',
        );
      }
      if (response.statusCode == HttpStatus.notFound) {
        throw GistException(
          'Настройки не найдены: gist удалён или версия Rebirth устарела. Установите свежую версию.',
        );
      }
      if (response.statusCode != HttpStatus.ok) {
        throw GistException(
          'GitHub ответил ошибкой ${response.statusCode}. Повторите позже.',
        );
      }
      json = (jsonDecode(text) as Map).cast<String, Object?>();
      newEtag = response.headers.value(HttpHeaders.etagHeader);
    } on GistException {
      rethrow;
    } on TimeoutException {
      throw GistException('GitHub не отвечает. Повторите позже.');
    } on SocketException {
      throw GistException('Нет связи с GitHub. Проверьте интернет.');
    } on HandshakeException {
      throw GistException(
        'Не удалось установить защищённое соединение с GitHub. Проверьте дату и время на устройстве.',
      );
    } on HttpException catch (e) {
      throw GistException(
        'Не удалось прочитать настройки. Повторите позже. (${e.message})',
      );
    } on FormatException {
      throw GistException(
        'GitHub ответил не так, как ожидалось. Повторите позже.',
      );
    }

    final files = json['files'];
    if (files is! Map) {
      throw GistException(
        'В gist нет настроек. Сообщите тому, кто ведёт серверы.',
      );
    }
    return GistSnapshot(
      config: await _content(files, 'c'),
      revocation: await _content(files, 'r'),
      release: await _content(files, 'u'),
      etag: newEtag,
    );
  }

  /// The text of one file. GitHub cuts large files short in the listing and
  /// gives their full address instead; ours are small, but it is cheap to be
  /// right.
  Future<String?> _content(Map<Object?, Object?> files, String name) async {
    final file = files[name];
    if (file is! Map) return null;
    if (file['truncated'] == true && file['raw_url'] is String) {
      try {
        final request = await _client
            .getUrl(Uri.parse(file['raw_url']! as String))
            .timeout(timeout);
        request.headers.set(HttpHeaders.userAgentHeader, 'awg-client');
        final response = await request.close().timeout(timeout);
        if (response.statusCode == HttpStatus.ok) {
          return (await response
                  .transform(utf8.decoder)
                  .join()
                  .timeout(timeout))
              .trim();
        }
      } on Exception {
        throw GistException('Не удалось дочитать настройки. Повторите позже.');
      }
    }
    final content = file['content'];
    return content is String ? content.trim() : null;
  }
}
