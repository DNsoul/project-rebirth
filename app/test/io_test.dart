// The parts that talk to the outside, run against real sockets on this machine:
// a local web server standing in for GitHub, and local UDP sockets standing in
// for the tunnel.

import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/app_state.dart';
import 'package:awg_client/core/bootstrap.dart';
import 'package:awg_client/core/dns_probe.dart';
import 'package:awg_client/core/gist_source.dart';
import 'package:awg_client/core/udp_enroll_transport.dart';
import 'package:awg_client/core/regions.dart' show regionLabel;
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('reading the gist', () {
    late HttpServer server;
    late GithubGistSource source;
    var status = 200;
    String? etag;
    Map<String, Object?> body = {};
    final requests = <HttpRequest>[];
    final seenHeaders = <Map<String, String>>[];

    setUp(() async {
      status = 200;
      etag = '"abc"';
      requests.clear();
      seenHeaders.clear();
      body = {
        'files': {
          'c': {'content': 'sealed-config\n', 'truncated': false},
          'r': {'content': 'sealed-revocation', 'truncated': false},
          'u': {'content': 'sealed-release\n', 'truncated': false},
          's': {'content': 'ignored', 'truncated': false},
        },
      };
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      server.listen((request) async {
        requests.add(request);
        seenHeaders.add({
          'user-agent': request.headers.value('user-agent') ?? '',
          'if-none-match': request.headers.value('if-none-match') ?? '',
          'accept': request.headers.value('accept') ?? '',
        });
        final response = request.response;
        if (request.uri.path == '/raw/c') {
          response
            ..statusCode = 200
            ..write('the-whole-config\n');
        } else if (request.headers.value('if-none-match') == etag &&
            status == 200) {
          response.statusCode = 304;
        } else {
          response.statusCode = status;
          if (etag != null) response.headers.set('etag', etag!);
          response.write(jsonEncode(body));
        }
        await response.close();
      });
      source = GithubGistSource(
        'gist-xyz',
        baseUrl: 'http://127.0.0.1:${server.port}',
      );
    });

    tearDown(() => server.close(force: true));

    test(
      'takes the configuration, the revocation and the announcement, and only those',
      () async {
        final snapshot = await source.fetch();
        expect(snapshot!.config, 'sealed-config');
        expect(snapshot.revocation, 'sealed-revocation');
        expect(snapshot.release, 'sealed-release');
        expect(snapshot.etag, '"abc"');
        expect(requests.single.uri.path, '/gists/gist-xyz');
      },
    );

    test('says who is asking, which GitHub insists on', () async {
      await source.fetch();
      expect(seenHeaders.single['user-agent'], isNotEmpty);
      expect(seenHeaders.single['accept'], contains('github'));
    });

    test(
      'is told "unchanged" when nothing is, and that costs nothing',
      () async {
        final first = await source.fetch();
        final second = await source.fetch(etag: first!.etag);
        expect(second, isNull);
        expect(seenHeaders.last['if-none-match'], '"abc"');
      },
    );

    test('reads a new version when the etag has moved on', () async {
      etag = '"def"';
      final snapshot = await source.fetch(etag: '"abc"');
      expect(snapshot, isNotNull);
      expect(snapshot!.etag, '"def"');
    });

    test('a gist without the files is not an error, just empty', () async {
      body = {'files': <String, Object?>{}};
      final snapshot = await source.fetch();
      expect(snapshot!.config, isNull);
      expect(snapshot.revocation, isNull);
      expect(snapshot.release, isNull);
    });

    test(
      'a file GitHub cut short is read in full from its own address',
      () async {
        body = {
          'files': {
            'c': {
              'content': 'sealed-conf',
              'truncated': true,
              'raw_url': 'http://127.0.0.1:${server.port}/raw/c',
            },
          },
        };
        expect((await source.fetch())!.config, 'the-whole-config');
      },
    );

    test(
      'a gist that is gone, a rate limit and a failure each say what happened',
      () async {
        status = 404;
        await expectLater(
          source.fetch(),
          throwsA(
            isA<GistException>().having(
              (e) => e.message,
              'm',
              contains('не найден'),
            ),
          ),
        );
        status = 403;
        await expectLater(
          source.fetch(),
          throwsA(
            isA<GistException>().having(
              (e) => e.message,
              'm',
              contains('лимит'),
            ),
          ),
        );
        status = 429;
        await expectLater(
          source.fetch(),
          throwsA(
            isA<GistException>().having(
              (e) => e.message,
              'm',
              contains('лимит'),
            ),
          ),
        );
        status = 500;
        await expectLater(
          source.fetch(),
          throwsA(
            isA<GistException>().having((e) => e.message, 'm', contains('500')),
          ),
        );
      },
    );

    test('an answer that is not JSON is reported, not crashed on', () async {
      // Not valid JSON, served as 200.
      await server.close(force: true);
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      server.listen((r) async {
        r.response.write('<html>captive portal</html>');
        await r.response.close();
      });
      final s = GithubGistSource(
        'g',
        baseUrl: 'http://127.0.0.1:${server.port}',
      );
      await expectLater(s.fetch(), throwsA(isA<GistException>()));
    });

    test('no network is reported as such', () async {
      final port = server.port;
      await server.close(force: true);
      final s = GithubGistSource(
        'g',
        baseUrl: 'http://127.0.0.1:$port',
        timeout: const Duration(seconds: 2),
      );
      await expectLater(
        s.fetch(),
        throwsA(
          // Either, depending on whether the refusal or the timeout comes first:
          // loopback refusals take a couple of seconds on Windows.
          isA<GistException>().having(
            (e) => e.message,
            'm',
            anyOf(contains('связи'), contains('не отвечает')),
          ),
        ),
      );
    });
  });

  group('the registration socket', () {
    late RawDatagramSocket peer;

    setUp(() async {
      peer = await RawDatagramSocket.bind(InternetAddress.loopbackIPv4, 0);
    });

    tearDown(() => peer.close());

    test('sends the datagram and returns the reply', () async {
      peer.listen((event) {
        if (event != RawSocketEvent.read) return;
        final d = peer.receive();
        if (d != null) {
          peer.send(
            utf8.encode('re:${utf8.decode(d.data)}'),
            d.address,
            d.port,
          );
        }
      });
      final t = UdpEnrollTransport(host: '127.0.0.1', port: peer.port);
      expect(await t.exchange('hello', const Duration(seconds: 2)), 're:hello');
      expect(
        await t.exchange('again', const Duration(seconds: 2)),
        're:again',
        reason: 'the socket is reused',
      );
      await t.close();
    });

    test('returns null when nothing comes back', () async {
      final t = UdpEnrollTransport(host: '127.0.0.1', port: peer.port);
      expect(
        await t.exchange('hello', const Duration(milliseconds: 300)),
        isNull,
      );
      await t.close();
    });
  });

  group('the data path probe', () {
    test('builds a well-formed question', () {
      final q = buildQuery(0x1234, 'one.one.one.one');
      expect(q.sublist(0, 4), [0x12, 0x34, 0x01, 0x00]);
      expect(q.sublist(4, 6), [0, 1], reason: 'one question');
      // 3 "one" 3 "one" 3 "one" 3 "one" 0, then type A and class IN
      expect(q.sublist(12), [
        3,
        111,
        110,
        101,
        3,
        111,
        110,
        101,
        3,
        111,
        110,
        101,
        3,
        111,
        110,
        101,
        0,
        0,
        1,
        0,
        1,
      ]);
    });

    test('recognises an answer to its own question and nothing else', () {
      Uint8List reply(int id, {bool response = true, int length = 12}) =>
          Uint8List(length)
            ..[0] = id >> 8
            ..[1] = id & 0xff
            ..[2] = response ? 0x81 : 0x01;
      expect(isAnswerTo(0x1234, reply(0x1234)), isTrue);
      expect(
        isAnswerTo(0x1234, reply(0x1235)),
        isFalse,
        reason: 'somebody else\'s answer',
      );
      expect(
        isAnswerTo(0x1234, reply(0x1234, response: false)),
        isFalse,
        reason: 'a question, not an answer',
      );
      expect(
        isAnswerTo(0x1234, reply(0x1234, length: 5)),
        isFalse,
        reason: 'too short to be DNS',
      );
    });

    test(
      'is satisfied by a resolver that answers, and gives up on one that does not',
      () async {
        final dns = await RawDatagramSocket.bind(
          InternetAddress.loopbackIPv4,
          0,
        );
        var answering = true;
        var asked = 0;
        dns.listen((event) {
          if (event != RawSocketEvent.read) return;
          final d = dns.receive();
          if (d == null) return;
          asked++;
          if (!answering) return;
          final out = Uint8List.fromList(d.data)..[2] = 0x81;
          dns.send(out, d.address, d.port);
        });
        final probe = UdpDnsProbe(port: dns.port);

        expect(
          await probe.resolverAnswers('127.0.0.1', const Duration(seconds: 3)),
          isTrue,
        );

        answering = false;
        asked = 0;
        final started = DateTime.now();
        expect(
          await probe.resolverAnswers(
            '127.0.0.1',
            const Duration(milliseconds: 2500),
          ),
          isFalse,
        );
        expect(
          asked,
          greaterThanOrEqualTo(2),
          reason: 'asked again, in case the first packet was lost',
        );
        expect(DateTime.now().difference(started).inSeconds, lessThan(6));

        expect(
          await probe.resolverAnswers(
            'not-an-address',
            const Duration(seconds: 1),
          ),
          isFalse,
        );
        dns.close();
      },
    );
  });

  group('what the app remembers', () {
    late Directory dir;

    setUp(() => dir = Directory.systemTemp.createTempSync('awgr-state-'));
    tearDown(() => dir.deleteSync(recursive: true));

    test('survives a restart', () async {
      final store = FileStateStore('${dir.path}\\sub\\state.json');
      final state = PersistedState(
        proto: const ProtoState(minEpoch: 4, lastSeq: 9),
        configBody: '{"a":1}',
        configEpoch: 4,
        etag: '"e"',
        deviceKey: 'key',
        addresses: {'server-1': '10.8.0.2/32'},
        lastAddress: '10.8.0.2/32',
      );
      await store.save(state);

      final back = await FileStateStore('${dir.path}\\sub\\state.json').load();
      expect(back.proto, const ProtoState(minEpoch: 4, lastSeq: 9));
      expect(back.configBody, '{"a":1}');
      expect(back.configEpoch, 4);
      expect(back.etag, '"e"');
      expect(back.deviceKey, 'key');
      expect(back.addresses, {'server-1': '10.8.0.2/32'});
      expect(back.lastAddress, '10.8.0.2/32');
    });

    test('leaves no half-written file behind', () async {
      final path = '${dir.path}\\state.json';
      final store = FileStateStore(path);
      await store.save(PersistedState());
      await store.save(PersistedState(lastAddress: 'x'));
      expect(File('$path.tmp').existsSync(), isFalse);
      expect(
        Directory(dir.path).listSync().map((e) => e.path.split('\\').last),
        ['state.json'],
      );
    });

    test(
      'nothing saved yet, or a file that is damaged, is an empty start',
      () async {
        final path = '${dir.path}\\state.json';
        expect((await FileStateStore(path).load()).deviceKey, isNull);

        File(path).writeAsStringSync('{ not json');
        expect((await FileStateStore(path).load()).lastAddress, isNull);

        File(path).writeAsStringSync('[1,2,3]');
        expect((await FileStateStore(path).load()).lastAddress, isNull);
      },
    );
  });

  group('what the build is made with', () {
    const sample =
        '{"root_pub":"r","backup_pub":"b","content_key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",'
        '"gist_id":"abc123","gist_owner":"someone"}';

    test('reads what awgctl writes', () {
      final b = Bootstrap.parse(sample);
      expect(b.gistId, 'abc123');
      expect(b.gistOwner, 'someone');
      expect(b.rootPub, 'r');
      expect(b.contentKey, hasLength(32));
      expect(b.trust.contentKey, b.contentKey);
    });

    test('refuses a build with something missing, saying what', () {
      expect(() => Bootstrap.parse('{}'), throwsA(isA<ProtoException>()));
      expect(
        () => Bootstrap.parse(sample.replaceAll('"gist_id":"abc123",', '')),
        throwsA(isA<ProtoException>()),
      );
      expect(() => Bootstrap.parse('nope'), throwsA(isA<ProtoException>()));
      expect(() => Bootstrap.parse('[1]'), throwsA(isA<ProtoException>()));
      expect(
        () => Bootstrap.parse(
          sample.replaceAll(
            'AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8',
            'AAEC',
          ),
        ),
        throwsA(isA<ProtoException>()),
        reason: 'a short content key',
      );
    });
  });

  group('place names', () {
    test('places', () {
      expect(regionLabel('sto'), 'Стокгольм');
      expect(regionLabel('waw'), 'Варшава');
      expect(
        regionLabel('xyz'),
        'xyz',
        reason: 'a region the app has not heard of is shown as it is',
      );
    });
  });
}
