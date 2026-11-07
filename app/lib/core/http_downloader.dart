import 'dart:async';
import 'dart:io';

import 'updater.dart';

/// Downloads over https, following redirects by hand: GitHub answers a release
/// download with a redirect to another host, and every step of it has to be
/// https too, which the client's own redirect handling does not insist on.
class HttpDownloader implements Downloader {
  HttpDownloader({
    HttpClient? client,
    this.timeout = const Duration(seconds: 30),
    this.maxRedirects = 5,
    this.allowPlainHttp = false,
  }) : _client = client ?? HttpClient();

  final Duration timeout;
  final int maxRedirects;

  /// For tests, which talk to a server on this machine. Never set in the apps.
  final bool allowPlainHttp;
  final HttpClient _client;

  static const _redirects = {
    HttpStatus.movedPermanently,
    HttpStatus.found,
    HttpStatus.seeOther,
    HttpStatus.temporaryRedirect,
    HttpStatus.permanentRedirect,
  };

  @override
  Future<void> download(
    Uri url,
    File file, {
    required int limit,
    void Function(int received)? onProgress,
  }) async {
    var at = url;
    try {
      for (var hop = 0; hop <= maxRedirects; hop++) {
        if (at.scheme != 'https' && !(allowPlainHttp && at.scheme == 'http')) {
          throw UpdateException(
            'Адрес для скачивания не защищён (${at.scheme}://). Отказываюсь качать.',
          );
        }
        final request = await _client.getUrl(at).timeout(timeout);
        request.followRedirects = false;
        request.headers.set(HttpHeaders.userAgentHeader, 'rebirth-updater');
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
          throw UpdateException(
            'Файл обновления не найден (ответ сервера ${response.statusCode}). Повторите позже.',
          );
        }
        await _save(response, file, limit, onProgress);
        return;
      }
      throw UpdateException(
        'Слишком много перенаправлений при скачивании. Повторите позже.',
      );
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
