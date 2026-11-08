import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/http_downloader.dart';
import 'package:awg_client/core/name_resolver.dart';
import 'package:awg_client/core/updater.dart';
import 'package:flutter_test/flutter_test.dart';

/// A resolver with a table, which says what it was asked.
class FakeResolver implements NameResolver {
  FakeResolver(this.table);

  final Map<String, List<String>> table;
  final asked = <String>[];

  @override
  Future<List<InternetAddress>> resolve(String host) async {
    asked.add(host);
    final found = table[host];
    if (found == null) throw SocketException('no address for $host');
    return [for (final ip in found) InternetAddress(ip)];
  }
}

void main() {
  late HttpServer server;
  late Directory dir;
  late File target;
  final requests = <String>[];
  final accepts = <String>[];
  final body = Uint8List.fromList(List.generate(100000, (i) => i % 253));

  Uri at(String path) => Uri.parse('http://127.0.0.1:${server.port}$path');
  HttpDownloader downloader({bool allowPlainHttp = true}) => HttpDownloader(
    allowPlainHttp: allowPlainHttp,
    timeout: const Duration(seconds: 2),
  );

  setUp(() async {
    requests.clear();
    accepts.clear();
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
        case '/busy':
          r.statusCode = HttpStatus.serviceUnavailable;
        case '/limited':
          r.statusCode = HttpStatus.forbidden;
        case '/elsewhere':
          r.statusCode = HttpStatus.found;
          r.headers.set(
            HttpHeaders.locationHeader,
            'http://other.test:${server.port}/file',
          );
        case '/repos/o/r/releases/tags/v1':
          r.write(
            jsonEncode({
              'assets': [
                {
                  'name': 'other.bin',
                  'url': 'http://127.0.0.1:${server.port}/asset/1',
                },
                {
                  'name': 'file.bin',
                  'url': 'http://127.0.0.1:${server.port}/asset/7',
                },
              ],
            }),
          );
        case '/repos/o/r/releases/tags/v2':
          r.write(jsonEncode({'assets': <Object>[]}));
        case '/repos/o/r/releases/tags/v3':
          r.write('<html>not json</html>');
        case '/asset/7':
          accepts.add(request.headers.value(HttpHeaders.acceptHeader) ?? '');
          if (request.headers.value(HttpHeaders.acceptHeader) !=
              'application/octet-stream') {
            r.statusCode = HttpStatus.unsupportedMediaType;
          } else {
            r.statusCode = HttpStatus.found;
            r.headers.set(HttpHeaders.locationHeader, '/file');
          }
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

  group('what a failure is called', () {
    test('a server that has trouble is not "not found"', () async {
      await expectLater(
        downloader().download(at('/busy'), target, limit: 10),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            allOf(
              contains('503'),
              contains('не отвечает'),
              isNot(contains('не найден')),
            ),
          ),
        ),
      );
    });

    test('a file that is not there is, and the code is shown', () async {
      await expectLater(
        downloader().download(at('/missing'), target, limit: 10),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            allOf(contains('404'), contains('не найден')),
          ),
        ),
      );
    });

    test('any other refusal says what it was', () async {
      await expectLater(
        downloader().download(at('/limited'), target, limit: 10),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            allOf(contains('403'), contains('не отдал')),
          ),
        ),
      );
    });
  });

  group('the route round the tunnel', () {
    HttpDownloader straight(FakeResolver resolver) => HttpDownloader(
      allowPlainHttp: true,
      timeout: const Duration(seconds: 2),
      resolver: resolver,
    );
    Uri named(String host, String path) =>
        Uri.parse('http://$host:${server.port}$path');

    test('finds the address of the name itself, not by the system', () async {
      final resolver = FakeResolver({
        'updates.test': ['127.0.0.1'],
      });
      // The system knows no such name: the usual route cannot reach it.
      await expectLater(
        straight(
          resolver,
        ).download(named('updates.test', '/file'), target, limit: body.length),
        throwsA(isA<UpdateException>()),
      );
      expect(resolver.asked, isEmpty, reason: 'the usual route does not ask');

      await straight(resolver).download(
        named('updates.test', '/file'),
        target,
        limit: body.length,
        route: DownloadRoute.direct,
      );
      expect(target.readAsBytesSync(), body);
      expect(resolver.asked, ['updates.test']);
    });

    test('goes on to the next address when one does not answer', () async {
      // Nothing listens on 127.0.0.2, which says no at once.
      final resolver = FakeResolver({
        'updates.test': ['127.0.0.2', '127.0.0.1'],
      });
      await straight(resolver).download(
        named('updates.test', '/file'),
        target,
        limit: body.length,
        route: DownloadRoute.direct,
      );
      expect(target.readAsBytesSync(), body);
    });

    test('finds the address again at every step of a redirect', () async {
      final resolver = FakeResolver({
        'updates.test': ['127.0.0.1'],
        'other.test': ['127.0.0.1'],
      });
      await straight(resolver).download(
        named('updates.test', '/elsewhere'),
        target,
        limit: body.length,
        route: DownloadRoute.direct,
      );
      expect(target.readAsBytesSync(), body);
      expect(resolver.asked, ['updates.test', 'other.test']);
    });

    test('says there is no connection when the name has no address', () async {
      await expectLater(
        straight(FakeResolver({})).download(
          named('updates.test', '/file'),
          target,
          limit: body.length,
          route: DownloadRoute.direct,
        ),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            contains('Нет связи'),
          ),
        ),
      );
    });

    test('still refuses an address that is not https', () async {
      await expectLater(
        HttpDownloader(
          resolver: FakeResolver({
            'updates.test': ['127.0.0.1'],
          }),
        ).download(
          named('updates.test', '/file'),
          target,
          limit: body.length,
          route: DownloadRoute.direct,
        ),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            contains('не защищён'),
          ),
        ),
      );
    });
  });

  group('which addresses a way is for', () {
    final d = HttpDownloader();
    final release = Uri.parse(
      'https://github.com/o/r/releases/download/v1/file.bin',
    );

    test('the usual way and the one round the tunnel are for any address', () {
      for (final url in [release, Uri.parse('https://mirror.example/f.exe')]) {
        expect(d.canUse(DownloadRoute.usual, url), isTrue);
        expect(d.canUse(DownloadRoute.direct, url), isTrue);
      }
    });

    test('the API is only for the release links of GitHub', () {
      expect(d.canUse(DownloadRoute.api, release), isTrue);
      for (final url in [
        'https://mirror.example/f.exe',
        'https://github.com/o/r/archive/v1.zip',
        'https://github.com.evil.example/o/r/releases/download/v1/file.bin',
        'https://gitlab.com/api/v4/projects/1/packages/generic/rebirth/1.0/f.exe',
      ]) {
        expect(d.canUse(DownloadRoute.api, Uri.parse(url)), isFalse, reason: url);
      }
    });
  });

  group('the route by the API', () {
    HttpDownloader viaApi() => HttpDownloader(
      allowPlainHttp: true,
      timeout: const Duration(seconds: 2),
      apiBase: Uri.parse('http://127.0.0.1:${server.port}'),
    );
    // The link as announced. It is never fetched on this route.
    Uri release(String tag, String name) =>
        Uri.parse('https://github.com/o/r/releases/download/$tag/$name');

    test(
      'finds the file by the release and its name, and asks for its bytes',
      () async {
        await viaApi().download(
          release('v1', 'file.bin'),
          target,
          limit: body.length,
          route: DownloadRoute.api,
        );
        expect(target.readAsBytesSync(), body);
        expect(requests, [
          '/repos/o/r/releases/tags/v1',
          '/asset/7',
          '/file',
        ], reason: 'the other file of the release is not touched');
        expect(
          accepts.single,
          'application/octet-stream',
          reason: 'the API gives the file only when asked for the bytes',
        );
      },
    );

    test('says so when the release does not have the file', () async {
      await expectLater(
        viaApi().download(
          release('v2', 'file.bin'),
          target,
          limit: 10,
          route: DownloadRoute.api,
        ),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            contains('file.bin'),
          ),
        ),
      );
    });

    test('says so when the release is not there', () async {
      await expectLater(
        viaApi().download(
          release('v404', 'file.bin'),
          target,
          limit: 10,
          route: DownloadRoute.api,
        ),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            contains('404'),
          ),
        ),
      );
    });

    test('does not trust an answer that is not JSON', () async {
      await expectLater(
        viaApi().download(
          release('v3', 'file.bin'),
          target,
          limit: 10,
          route: DownloadRoute.api,
        ),
        throwsA(isA<UpdateException>()),
      );
    });

    test(
      'has no road for an address that is not GitHub\'s release link',
      () async {
        for (final url in [
          'https://example.org/file.bin',
          'https://github.com/o/r/archive/v1.zip',
          'https://github.com.evil.example/o/r/releases/download/v1/file.bin',
        ]) {
          await expectLater(
            viaApi().download(
              Uri.parse(url),
              target,
              limit: 10,
              route: DownloadRoute.api,
            ),
            throwsA(isA<UpdateException>()),
            reason: url,
          );
        }
        expect(requests, isEmpty, reason: 'nothing was asked of anyone');
      },
    );

    test('still stops a file that is bigger than announced', () async {
      await expectLater(
        viaApi().download(
          release('v1', 'file.bin'),
          target,
          limit: body.length ~/ 2,
          route: DownloadRoute.api,
        ),
        throwsA(
          isA<UpdateException>().having(
            (e) => e.message,
            'message',
            contains('больше, чем объявлено'),
          ),
        ),
      );
    });
  });
}
