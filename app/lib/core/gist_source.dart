import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data' show BytesBuilder;

import 'bootstrap.dart';

/// What was in the gist at the moment it was read.
class GistSnapshot {
  const GistSnapshot({
    this.config,
    this.revocation,
    this.release,
    this.etag,
    this.others = const [],
  });

  /// The sealed configuration (file `c`), or null if the gist has none.
  final String? config;

  /// The sealed revocation record (file `r`).
  final String? revocation;

  /// The sealed announcement of a new version (file `u`).
  final String? release;

  /// Identifies this version of the gist, so asking again can be answered with
  /// "no change" without spending a request.
  final String? etag;

  /// What other places answered, when the files are kept in more than one. They
  /// are to be looked at together with this one: of each file, whichever version
  /// is newest is the one that counts.
  final List<GistSnapshot> others;
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

/// The same signed files as in the gist, read from a public address: a package in
/// a GitLab project, where the server and the operator keep them next to the
/// gist. [template] is the address with `{file}` in it.
class HttpFileMirror implements GistSource {
  HttpFileMirror(
    this.template, {
    HttpClient? client,
    this.name = 'GitLab',
    this.timeout = const Duration(seconds: 20),
  }) : _client = client ?? HttpClient();

  final String template;
  final String name;
  final Duration timeout;
  final HttpClient _client;

  /// The files are a few hundred bytes. Anything much bigger is not ours.
  static const _maxBytes = 256 * 1024;

  @override
  Future<GistSnapshot?> fetch({String? etag}) async {
    final List<String?> files;
    try {
      files = await Future.wait([_read('c'), _read('r'), _read('u')]);
    } on GistException {
      rethrow;
    } on TimeoutException {
      throw GistException('$name не отвечает.');
    } on SocketException {
      throw GistException('Нет связи с $name.');
    } on HandshakeException {
      throw GistException(
        'Не удалось установить защищённое соединение с $name. Проверьте дату и время на устройстве.',
      );
    } on HttpException catch (e) {
      throw GistException(
        'Не удалось прочитать настройки с $name. (${e.message})',
      );
    }

    // The place does not say whether anything changed, so it is worked out from
    // what it holds: the same files, the same answer.
    final tag = _fingerprint(files);
    if (etag != null && etag == tag) return null;
    return GistSnapshot(
      config: files[0],
      revocation: files[1],
      release: files[2],
      etag: tag,
    );
  }

  /// One file, or null when it is not there (or there is nothing in it).
  Future<String?> _read(String file) async {
    final request = await _client
        .getUrl(Uri.parse(template.replaceAll('{file}', file)))
        .timeout(timeout);
    request.headers.set(HttpHeaders.userAgentHeader, 'awg-client');
    final response = await request.close().timeout(timeout);
    if (response.statusCode == HttpStatus.notFound) {
      await response.drain<void>();
      return null;
    }
    if (response.statusCode != HttpStatus.ok) {
      await response.drain<void>();
      throw GistException('$name ответил ошибкой ${response.statusCode}.');
    }
    final bytes = BytesBuilder(copy: false);
    await for (final chunk in response.timeout(timeout)) {
      bytes.add(chunk);
      if (bytes.length > _maxBytes) {
        throw GistException('$name отдал не то, что ожидалось.');
      }
    }
    final text = utf8.decode(bytes.takeBytes(), allowMalformed: true).trim();
    return text.isEmpty ? null : text;
  }

  static String _fingerprint(List<String?> files) {
    // FNV-1a over everything, with the lengths in front so that moving a byte
    // from one file to the next changes the answer too.
    var hash = 0x811c9dc5;
    for (final f in files) {
      for (final unit in utf8.encode('${f?.length ?? -1}:${f ?? ''}\n')) {
        hash = ((hash ^ unit) * 0x01000193) & 0xffffffff;
      }
    }
    return '${files.map((f) => f?.length ?? 0).join('.')}.${hash.toRadixString(16)}';
  }
}

/// Reads the same files from several places that are equals of each other and
/// hands back everything that is new. The caller verifies each of them and takes
/// the newest version of each file, which the anti-rollback checks it already
/// does make independent of the order.
///
/// One place being slow or down costs nothing: once one has answered, the others
/// are only waited for a few seconds more. A place that has not answered by then
/// is asked again at the next look.
class MirroredSource implements GistSource {
  MirroredSource(this.sources, {this.straggler = const Duration(seconds: 4)})
    : assert(sources.length > 1, 'one place needs no mirroring');

  final List<GistSource> sources;

  /// How long the others are waited for after the first one has answered.
  final Duration straggler;

  static const _separator = '\t';

  @override
  Future<GistSnapshot?> fetch({String? etag}) async {
    final n = sources.length;
    final tags = _split(etag, n);
    final outcomes = List<_Outcome?>.filled(n, null);
    final done = Completer<void>();
    Timer? grace;
    var pending = n;

    for (var i = 0; i < n; i++) {
      sources[i]
          .fetch(etag: tags[i])
          .then<_Outcome>(_Outcome.answered, onError: _Outcome.failed)
          .then((outcome) {
            if (done.isCompleted) return; // too late: left for the next look
            outcomes[i] = outcome;
            pending--;
            if (pending == 0) {
              grace?.cancel();
              done.complete();
            } else if (outcome.error == null && grace == null) {
              grace = Timer(straggler, () {
                if (!done.isCompleted) done.complete();
              });
            }
          });
    }
    await done.future;

    final answered = <int>[];
    final problems = <String>[];
    for (var i = 0; i < n; i++) {
      final outcome = outcomes[i];
      if (outcome == null) continue;
      if (outcome.error == null) {
        answered.add(i);
      } else {
        problems.add(outcome.error!);
      }
    }
    if (answered.isEmpty) {
      throw GistException(problems.join(' '));
    }

    final next = [...tags];
    final changed = <GistSnapshot>[];
    for (final i in answered) {
      final snapshot = outcomes[i]!.snapshot;
      if (snapshot == null) continue; // "unchanged"
      next[i] = snapshot.etag;
      changed.add(snapshot);
    }
    if (changed.isEmpty) return null;

    final first = changed.first;
    return GistSnapshot(
      config: first.config,
      revocation: first.revocation,
      release: first.release,
      etag: next.map((t) => t ?? '').join(_separator),
      others: [
        for (final s in changed.skip(1))
          GistSnapshot(
            config: s.config,
            revocation: s.revocation,
            release: s.release,
          ),
      ],
    );
  }

  /// The saved etag holds one part per place. One that does not fit (saved by a
  /// build that read the gist alone) is as good as none: everything is read.
  static List<String?> _split(String? etag, int n) {
    if (etag == null || etag.isEmpty) return List.filled(n, null);
    final parts = etag.split(_separator);
    if (parts.length != n) return List.filled(n, null);
    return [for (final p in parts) p.isEmpty ? null : p];
  }
}

class _Outcome {
  _Outcome._(this.snapshot, this.error);

  static _Outcome answered(GistSnapshot? snapshot) =>
      _Outcome._(snapshot, null);

  static _Outcome failed(Object error, [StackTrace? _]) => _Outcome._(
    null,
    error is GistException ? error.message : error.toString(),
  );

  final GistSnapshot? snapshot;
  final String? error;
}

/// Where the app reads the signed files from: the gist, and the other places the
/// build knows of, if any.
GistSource gistSourceFor(Bootstrap bootstrap) {
  final github = GithubGistSource(bootstrap.gistId);
  if (bootstrap.configMirrors.isEmpty) return github;
  return MirroredSource([
    github,
    for (final template in bootstrap.configMirrors) HttpFileMirror(template),
  ]);
}
