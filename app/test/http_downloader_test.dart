import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/http_downloader.dart';
import 'package:awg_client/core/updater.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  late HttpServer server;
  late Directory dir;
  late File target;
  final requests = <String>[];
  final body = Uint8List.fromList(List.generate(100000, (i) => i % 253));

  Uri at(String path) => Uri.parse('http://127.0.0.1:${server.port}$path');
  HttpDownloader downloader({bool allowPlainHttp = true}) => HttpDownloader(
    allowPlainHttp: allowPlainHttp,
    timeout: const Duration(seconds: 2),
  );

  setUp(() async {
    requests.clear();
    dir = Directory.systemTemp.createTempSync('rebirth-download-test');
    target = File('${dir.path}${Platform.pathSeparator}file.part');
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((request) async {
      final path = request.uri.path;
      requests.add(path);
      final r = request.response;
      switch (path) {
        case '/file':
          r.add(body);
        case '/old':
          r.statusCode = HttpStatus.movedPermanently;
          r.headers.set(HttpHeaders.locationHeader, '/file');
        case '/hop1':
          r.statusCode = HttpStatus.found;
          r.headers.set(HttpHeaders.locationHeader, '/hop2');
        case '/hop2':
          r.statusCode = HttpStatus.temporaryRedirect;
          r.headers.set(
            HttpHeaders.locationHeader,
            'http://127.0.0.1:${server.port}/file',
          );
        case '/loop':
          r.statusCode = HttpStatus.found;
          r.headers.set(HttpHeaders.locationHeader, '/loop');
        case '/nowhere':
          r.statusCode = HttpStatus.found;
        case '/to-plain-from-secure':
          r.statusCode = HttpStatus.found;
          r.headers.set(HttpHeaders.locationHeader, 'ftp://example.org/x');
        case '/huge':
          r.add(Uint8List(body.length * 3));
        case '/stall':
          r.add([1, 2, 3]);
          await r.flush();
          return; // never finishes
        default:
          r.statusCode = HttpStatus.notFound;
      }
      await r.close();
    });
  });

  tearDown(() async {
    await server.close(force: true);
    dir.deleteSync(recursive: true);
  });

  test('saves the body, and says how far it got', () async {
    final seen = <int>[];
    await downloader().download(
      at('/file'),
      target,
      limit: body.length,
      onProgress: seen.add,
    );

    expect(target.readAsBytesSync(), body);
    expect(seen, isNotEmpty);
    expect(seen.last, body.length);
    expect(seen, orderedEquals([...seen]..sort()));
  });

  test('follows redirects, even a chain of them', () async {
    await downloader().download(at('/old'), target, limit: body.length);
    expect(target.readAsBytesSync(), body);

    await downloader().download(at('/hop1'), target, limit: body.length);
    expect(requests, containsAllInOrder(['/hop1', '/hop2', '/file']));
  });

  test('gives up on a redirect that goes round in circles', () async {
    await expectLater(
      downloader().download(at('/loop'), target, limit: 10),
      throwsA(
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          contains('перенаправлений'),
        ),
      ),
    );
  });

  test('gives up on a redirect that says nothing about where to', () async {
    await expectLater(
      downloader().download(at('/nowhere'), target, limit: 10),
      throwsA(isA<UpdateException>()),
    );
  });

  test('does not follow a redirect to something that is not https', () async {
    // The test server is plain http on purpose; a redirect to ftp must still stop.
    await expectLater(
      downloader().download(at('/to-plain-from-secure'), target, limit: 10),
      throwsA(
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          contains('не защищён'),
        ),
      ),
    );
  });

  test('refuses an address that is not https at all, without asking', () async {
    await expectLater(
      downloader(
        allowPlainHttp: false,
      ).download(at('/file'), target, limit: body.length),
      throwsA(
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          contains('не защищён'),
        ),
      ),
    );
    expect(
      requests,
      isEmpty,
      reason: 'nothing was sent to a server that is not https',
    );
  });

  test('reports a missing file in words', () async {
    await expectLater(
      downloader().download(at('/missing'), target, limit: 10),
      throwsA(
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          contains('404'),
        ),
      ),
    );
  });

  test('stops a server that sends more than was announced', () async {
    await expectLater(
      downloader().download(at('/huge'), target, limit: body.length),
      throwsA(
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          contains('больше, чем объявлено'),
        ),
      ),
    );
    expect(
      target.lengthSync(),
      lessThanOrEqualTo(body.length),
      reason: 'what was written never went past the announced size',
    );
  });

  test('gives up on a server that falls silent halfway', () async {
    await expectLater(
      downloader().download(at('/stall'), target, limit: body.length),
      throwsA(isA<UpdateException>()),
    );
  });

  test('reports a server that is not there', () async {
    final dead = Uri.parse('http://127.0.0.1:${server.port}/file');
    await server.close(force: true);
    await expectLater(
      downloader().download(dead, target, limit: 10),
      throwsA(
        // Windows may take longer than the timeout to report a refused
        // connection, so either answer is the right one.
        isA<UpdateException>().having(
          (e) => e.message,
          'message',
          anyOf(contains('Нет связи'), contains('не отвечает')),
        ),
      ),
    );
    server = await HttpServer.bind(
      InternetAddress.loopbackIPv4,
      0,
    ); // for tearDown
  });
}
