import 'dart:convert';
import 'dart:io';

import 'package:awg_client/core/name_resolver.dart';
import 'package:flutter_test/flutter_test.dart';

/// A public resolver that answers in JSON over plain http, on this machine.
class FakeDoh {
  FakeDoh._(this.server);

  static Future<FakeDoh> start(
    Object Function(Uri query) answer, {
    int status = 200,
  }) async {
    final doh = FakeDoh._(
      await HttpServer.bind(InternetAddress.loopbackIPv4, 0),
    );
    doh.server.listen((request) async {
      doh.queries.add(request.uri);
      doh.accepts.add(request.headers.value(HttpHeaders.acceptHeader) ?? '');
      request.response.statusCode = status;
      final a = answer(request.uri);
      request.response.write(a is String ? a : jsonEncode(a));
      await request.response.close();
    });
    return doh;
  }

  final HttpServer server;
  final queries = <Uri>[];
  final accepts = <String>[];

  Uri get url => Uri.parse('http://127.0.0.1:${server.port}/dns-query');

  Future<void> close() => server.close(force: true);
}

Map<String, Object?> a(String ip) => {
  'name': 'x',
  'type': 1,
  'TTL': 60,
  'data': ip,
};

void main() {
  final open = <FakeDoh>[];
  Future<FakeDoh> doh(Object Function(Uri) answer, {int status = 200}) async {
    final d = await FakeDoh.start(answer, status: status);
    open.add(d);
    return d;
  }

  tearDown(() async {
    for (final d in open) {
      await d.close();
    }
    open.clear();
  });

  DohResolver resolver(List<FakeDoh> servers) => DohResolver(
    servers: [for (final s in servers) s.url],
    timeout: const Duration(seconds: 2),
  );

  test('asks for the addresses of the name, as JSON', () async {
    final d = await doh(
      (_) => {
        'Status': 0,
        'Answer': [a('140.82.114.3')],
      },
    );
    final found = await resolver([d]).resolve('github.com');

    expect(found.map((x) => x.address), ['140.82.114.3']);
    expect(d.queries.single.queryParameters, {
      'name': 'github.com',
      'type': 'A',
    });
    expect(d.accepts.single, 'application/dns-json');
  });

  test(
    'takes only the addresses out of an answer that has more in it',
    () async {
      final d = await doh(
        (_) => {
          'Status': 0,
          'Answer': [
            {'name': 'x', 'type': 5, 'data': 'other.example.'},
            a('185.199.108.133'),
            {'name': 'x', 'type': 28, 'data': '2606:50c0:8000::154'},
            a('185.199.109.133'),
          ],
        },
      );
      final found = await resolver([d]).resolve('x');
      expect(found.map((x) => x.address), [
        '185.199.108.133',
        '185.199.109.133',
      ]);
    },
  );

  test(
    'puts the answers of all the servers together, without repeats',
    () async {
      final one = await doh(
        (_) => {
          'Answer': [a('1.1.1.10'), a('1.1.1.11')],
        },
      );
      final two = await doh(
        (_) => {
          'Answer': [a('1.1.1.11'), a('1.1.1.12')],
        },
      );
      final found = await resolver([one, two]).resolve('x');
      expect(found.map((x) => x.address).toSet(), {
        '1.1.1.10',
        '1.1.1.11',
        '1.1.1.12',
      });
      expect(found, hasLength(3));
    },
  );

  test('is not stopped by a server that fails, or talks nonsense', () async {
    final broken = await doh((_) => 'this is not json');
    final gone = await doh((_) => 'nope', status: 500);
    final good = await doh(
      (_) => {
        'Answer': [a('9.9.9.9')],
      },
    );
    final found = await resolver([broken, gone, good]).resolve('x');
    expect(found.map((x) => x.address), ['9.9.9.9']);
  });

  test('is not stopped by a server that is not there at all', () async {
    final dead = await doh((_) => {});
    final good = await doh(
      (_) => {
        'Answer': [a('9.9.9.9')],
      },
    );
    final deadUrl = dead.url;
    await dead.close();
    final r = DohResolver(
      servers: [deadUrl, good.url],
      timeout: const Duration(seconds: 2),
    );
    expect((await r.resolve('x')).map((x) => x.address), ['9.9.9.9']);
  });

  test('does not hand out addresses that cannot be the name\'s', () async {
    final d = await doh(
      (_) => {
        'Answer': [
          a('127.0.0.1'),
          a('169.254.1.1'),
          a('224.0.0.1'),
          a('not an address'),
          a('8.8.4.4'),
        ],
      },
    );
    final found = await resolver([d]).resolve('x');
    expect(found.map((x) => x.address), ['8.8.4.4']);
  });

  test('says so when nobody knows the name', () async {
    final d = await doh((_) => {'Status': 3});
    await expectLater(
      resolver([d]).resolve('nowhere.example'),
      throwsA(isA<SocketException>()),
    );
  });

  test('an address needs no asking', () async {
    final d = await doh(
      (_) => {
        'Answer': [a('1.2.3.4')],
      },
    );
    final found = await resolver([d]).resolve('203.0.113.7');
    expect(found.single.address, '203.0.113.7');
    expect(d.queries, isEmpty);
  });

  test('gives no more than a handful of addresses', () async {
    final d = await doh(
      (_) => {
        'Answer': [for (var i = 1; i <= 20; i++) a('10.9.0.$i')],
      },
    );
    expect(await resolver([d]).resolve('x'), hasLength(8));
  });
}
