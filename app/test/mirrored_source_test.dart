import 'dart:async';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/bootstrap.dart';
import 'package:awg_client/core/gist_source.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter_test/flutter_test.dart';

/// A place that answers what the test tells it to.
class _Place implements GistSource {
  GistSnapshot? answer;
  Object? error;
  Completer<void>? gate;
  int calls = 0;
  String? askedWith;

  @override
  Future<GistSnapshot?> fetch({String? etag}) async {
    calls++;
    askedWith = etag;
    final g = gate;
    if (g != null) await g.future;
    if (error != null) throw error!;
    return answer;
  }
}

void main() {
  group('reading the same files from a public address', () {
    late HttpServer server;
    final files = <String, String>{};
    var requests = <String>[];
    var status = HttpStatus.ok;
    late HttpFileMirror mirror;

    setUp(() async {
      files.clear();
      requests = [];
      status = HttpStatus.ok;
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      server.listen((request) async {
        requests.add(request.uri.path);
        final response = request.response;
        // The real registry answers a download with a redirect to storage.
        if (request.uri.path.startsWith(
          '/api/v4/projects/7/packages/generic/sync/1/',
        )) {
          final name = request.uri.pathSegments.last;
          response.statusCode = HttpStatus.found;
          response.headers.set('location', '/storage/$name');
          await response.close();
          return;
        }
        if (status != HttpStatus.ok) {
          response.statusCode = status;
          await response.close();
          return;
        }
        final name = request.uri.pathSegments.last;
        final body = files[name];
        if (body == null) {
          response.statusCode = HttpStatus.notFound;
        } else {
          response.write(body);
        }
        await response.close();
      });
      mirror = HttpFileMirror(
        'http://127.0.0.1:${server.port}/api/v4/projects/7/packages/generic/sync/1/{file}',
        timeout: const Duration(seconds: 5),
      );
    });

    tearDown(() => server.close(force: true));

    test(
      'takes what is there, follows the redirect, and leaves the rest empty',
      () async {
        files['c'] = 'config-blob\n';
        files['u'] = 'release-blob';

        final snapshot = await mirror.fetch();
        expect(snapshot, isNotNull);
        expect(snapshot!.config, 'config-blob');
        expect(snapshot.release, 'release-blob');
        expect(snapshot.revocation, isNull, reason: 'a file that is not there');
        expect(snapshot.etag, isNotEmpty);
        expect(
          requests.where((p) => p.startsWith('/storage/')).length,
          3,
          reason: 'all three files were asked for, through the redirect',
        );
      },
    );

    test('says "unchanged" for the same files and notices a new one', () async {
      files['c'] = 'one';
      final first = await mirror.fetch();
      expect(await mirror.fetch(etag: first!.etag), isNull);

      files['c'] = 'two';
      final second = await mirror.fetch(etag: first.etag);
      expect(second, isNotNull);
      expect(second!.config, 'two');
      expect(second.etag, isNot(first.etag));

      // The same bytes moved from one file to the next are not "unchanged".
      files
        ..clear()
        ..['c'] = 'ab'
        ..['r'] = 'c';
      final a = await mirror.fetch();
      files
        ..clear()
        ..['c'] = 'a'
        ..['r'] = 'bc';
      final b = await mirror.fetch(etag: a!.etag);
      expect(b, isNotNull);
    });

    test('an empty place is an answer, not an error', () async {
      final snapshot = await mirror.fetch();
      expect(snapshot, isNotNull);
      expect(snapshot!.config, isNull);
    });

    test('a failing place is reported by name, in words', () async {
      status = HttpStatus.badGateway;
      await expectLater(
        mirror.fetch(),
        throwsA(
          isA<GistException>().having(
            (e) => e.message,
            'message',
            allOf(contains('GitLab'), contains('502')),
          ),
        ),
      );

      final gone = HttpFileMirror(
        'http://127.0.0.1:1/{file}',
        timeout: const Duration(seconds: 2),
      );
      await expectLater(
        gone.fetch(),
        throwsA(
          isA<GistException>().having(
            (e) => e.message,
            'message',
            contains('GitLab'),
          ),
        ),
      );
    });

    test('refuses an answer far bigger than the files are', () async {
      files['c'] = 'x' * (300 * 1024);
      await expectLater(mirror.fetch(), throwsA(isA<GistException>()));
    });
  });

  group('reading from several places at once', () {
    late _Place github;
    late _Place gitlab;
    late MirroredSource source;

    setUp(() {
      github = _Place();
      gitlab = _Place();
      source = MirroredSource([
        github,
        gitlab,
      ], straggler: const Duration(milliseconds: 30));
    });

    test('hands back what each place has, with one etag for all', () async {
      github.answer = const GistSnapshot(config: 'from-github', etag: 'g1');
      gitlab.answer = const GistSnapshot(config: 'from-gitlab', etag: 'l1');

      final snapshot = await source.fetch();
      expect(snapshot!.config, 'from-github');
      expect(snapshot.others.single.config, 'from-gitlab');
      expect(snapshot.etag, 'g1\tl1');
    });

    test('asks each place with its own part of the etag', () async {
      github.answer = null;
      gitlab.answer = null;
      expect(await source.fetch(etag: 'g1\tl1'), isNull);
      expect(github.askedWith, 'g1');
      expect(gitlab.askedWith, 'l1');
    });

    test(
      'an etag from before there were two places is as good as none',
      () async {
        github.answer = null;
        gitlab.answer = null;
        await source.fetch(etag: 'W/"abc"');
        expect(github.askedWith, isNull);
        expect(gitlab.askedWith, isNull);
        await source.fetch();
        expect(github.askedWith, isNull);
      },
    );

    test(
      'only what changed comes back, and the etag keeps the other part',
      () async {
        github.answer = null; // unchanged
        gitlab.answer = const GistSnapshot(config: 'newer', etag: 'l2');

        final snapshot = await source.fetch(etag: 'g1\tl1');
        expect(snapshot!.config, 'newer');
        expect(snapshot.others, isEmpty);
        expect(snapshot.etag, 'g1\tl2');
      },
    );

    test('nothing changed anywhere is "unchanged"', () async {
      github.answer = null;
      gitlab.answer = null;
      expect(await source.fetch(etag: 'g1\tl1'), isNull);
    });

    test('one place being down costs nothing', () async {
      github.error = GistException('Нет связи с GitHub.');
      gitlab.answer = const GistSnapshot(config: 'c', etag: 'l1');

      final snapshot = await source.fetch(etag: 'g0\t');
      expect(snapshot!.config, 'c');
      expect(
        snapshot.etag,
        'g0\tl1',
        reason: 'the place that did not answer keeps what was known of it',
      );

      // And when the place that is up has nothing new, that is the answer.
      gitlab.answer = null;
      expect(await source.fetch(etag: 'g0\tl1'), isNull);
    });

    test('every place being down is one failure that names them all', () async {
      github.error = GistException('Нет связи с GitHub.');
      gitlab.error = GistException('GitLab не отвечает.');
      await expectLater(
        source.fetch(),
        throwsA(
          isA<GistException>().having(
            (e) => e.message,
            'message',
            allOf(contains('GitHub'), contains('GitLab')),
          ),
        ),
      );
    });

    test(
      'a place that hangs is not waited for once another has answered',
      () async {
        github.gate = Completer<void>(); // never answers until told to
        gitlab.answer = const GistSnapshot(config: 'quick', etag: 'l1');

        final started = DateTime.now();
        final snapshot = await source.fetch();
        expect(snapshot!.config, 'quick');
        expect(
          DateTime.now().difference(started),
          lessThan(const Duration(seconds: 2)),
        );
        expect(snapshot.others, isEmpty);

        // When it does answer, nobody is waiting and nothing breaks.
        github.gate!.complete();
        await Future<void>.delayed(const Duration(milliseconds: 20));
      },
    );

    test('a place that is slow but not hung is still heard', () async {
      github.gate = Completer<void>();
      github.answer = const GistSnapshot(config: 'slow', etag: 'g1');
      gitlab.answer = const GistSnapshot(config: 'quick', etag: 'l1');
      Timer(const Duration(milliseconds: 5), github.gate!.complete);

      final snapshot = await source.fetch();
      expect(snapshot!.others, hasLength(1));
    });

    test('a place with only one place configured is just that place', () {
      final gist = gistSourceFor(
        Bootstrap(
          gistId: 'g',
          rootPub: 'r',
          backupPub: 'b',
          contentKey: Uint8List(32),
        ),
      );
      expect(gist, isA<GithubGistSource>());

      final both = gistSourceFor(
        Bootstrap(
          gistId: 'g',
          rootPub: 'r',
          backupPub: 'b',
          contentKey: Uint8List(32),
          configMirrors: const ['https://gitlab.com/x/{file}'],
        ),
      );
      expect(both, isA<MirroredSource>());
    });
  });

  group('what the build is made with', () {
    Map<String, Object?> data() => {
      'gist_id': 'abc',
      'root_pub': 'r',
      'backup_pub': 'b',
      'content_key': encode(Uint8List(contentKeySize)),
    };

    test('names the other places for the files, when it has any', () {
      expect(Bootstrap.fromJson(data()).configMirrors, isEmpty);

      final b = Bootstrap.fromJson({
        ...data(),
        'config_mirrors': [
          'https://gitlab.com/api/v4/projects/5/packages/generic/sync/1/{file}',
        ],
      });
      expect(b.configMirrors, hasLength(1));
      expect(b.releaseMirrors, isEmpty);
    });

    test('refuses a mistake in them loudly', () {
      for (final bad in [
        'not a list',
        ['http://gitlab.com/{file}'],
        ['https://gitlab.com/no-file-here'],
        [42],
      ]) {
        expect(
          () => Bootstrap.fromJson({...data(), 'config_mirrors': bad}),
          throwsA(
            isA<ProtoException>().having(
              (e) => e.message,
              'message',
              contains('config_mirrors'),
            ),
          ),
          reason: '$bad',
        );
      }
    });
  });
}
