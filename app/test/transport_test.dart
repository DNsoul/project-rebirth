// The registration socket against the failure that stopped the first real run:
// Windows reports "host unreachable" (error 1232) from the socket while the
// tunnel's route is still being set up.

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/dns_probe.dart';
import 'package:awg_client/core/udp_enroll_transport.dart';
import 'package:flutter_test/flutter_test.dart';

/// A datagram socket whose behaviour the test decides. Anything the transport
/// does not use is left unimplemented.
class FakeSocket extends Stream<RawSocketEvent> implements RawDatagramSocket {
  FakeSocket({this.onSend});

  final void Function(FakeSocket self, List<int> data)? onSend;
  final _events = StreamController<RawSocketEvent>(sync: true);
  final sent = <String>[];
  Datagram? _pending;
  Object? _receiveError;
  bool closed = false;

  @override
  StreamSubscription<RawSocketEvent> listen(
    void Function(RawSocketEvent event)? onData, {
    Function? onError,
    void Function()? onDone,
    bool? cancelOnError,
  }) => _events.stream.listen(
    onData,
    onError: onError,
    onDone: onDone,
    cancelOnError: cancelOnError,
  );

  /// The next `receive()` throws [error], as the real one did.
  void failNextReceive(Object error) {
    _receiveError = error;
    _events.add(RawSocketEvent.read);
  }

  /// Something arrives.
  void deliver(String text) {
    _pending = Datagram(utf8.encode(text), InternetAddress.loopbackIPv4, 7843);
    _events.add(RawSocketEvent.read);
  }

  /// Something arrives, as raw bytes.
  void deliverBytes(List<int> data) {
    _pending = Datagram(
      Uint8List.fromList(data),
      InternetAddress.loopbackIPv4,
      53,
    );
    _events.add(RawSocketEvent.read);
  }

  /// An error on the event stream itself.
  void failStream(Object error) => _events.addError(error);

  @override
  int send(List<int> buffer, InternetAddress address, int port) {
    // Latin-1 never fails, and a DNS query is bytes, not text.
    sent.add(latin1.decode(buffer));
    onSend?.call(this, buffer);
    return buffer.length;
  }

  @override
  Datagram? receive() {
    final error = _receiveError;
    if (error != null) {
      _receiveError = null;
      throw error;
    }
    final d = _pending;
    _pending = null;
    return d;
  }

  @override
  void close() {
    closed = true;
    unawaited(_events.close());
  }

  @override
  dynamic noSuchMethod(Invocation invocation) => super.noSuchMethod(invocation);
}

SocketException hostUnreachable() => SocketException(
  'Сетевая папка недоступна.',
  osError: const OSError('Сетевая папка недоступна.', 1232),
  address: InternetAddress.anyIPv4,
  port: 0,
);

void main() {
  const wait = Duration(milliseconds: 150);

  test(
    'an error from receive() is not an unhandled exception, and the next attempt works',
    () async {
      final errors = <Object>[];
      late final UdpEnrollTransport transport;
      final sockets = <FakeSocket>[];

      await runZonedGuarded(() async {
        transport = UdpEnrollTransport(
          bind: () async {
            final index = sockets.length;
            final socket = FakeSocket(
              onSend: (self, data) {
                // The first socket is told the host is unreachable, the way
                // Windows does while the route is not there yet. The second
                // one is on a working tunnel and gets an answer.
                if (index == 0) {
                  self.failNextReceive(hostUnreachable());
                } else {
                  self.deliver('reply-to-${utf8.decode(data)}');
                }
              },
            );
            sockets.add(socket);
            return socket;
          },
        );

        expect(
          await transport.exchange('one', wait),
          isNull,
          reason: 'the failed attempt',
        );
        expect(sockets, hasLength(1));
        expect(
          sockets.first.closed,
          isTrue,
          reason: 'a socket that reported the error is not trusted again',
        );

        expect(await transport.exchange('two', wait), 'reply-to-two');
        expect(
          sockets,
          hasLength(2),
          reason: 'a fresh socket for the next attempt',
        );
        await transport.close();
      }, (e, s) => errors.add(e));

      expect(
        errors,
        isEmpty,
        reason: 'the first run died of exactly this: $errors',
      );
      expect(transport.socketErrors, 1);
      expect(transport.describe(), contains('1232'));
    },
  );

  test('an error on the event stream is handled the same way', () async {
    final errors = <Object>[];
    final sockets = <FakeSocket>[];
    String? second;

    await runZonedGuarded(() async {
      final transport = UdpEnrollTransport(
        bind: () async {
          final index = sockets.length;
          final socket = FakeSocket(
            onSend: (self, data) {
              if (index == 0) {
                self.failStream(hostUnreachable());
              } else {
                self.deliver('ok');
              }
            },
          );
          sockets.add(socket);
          return socket;
        },
      );
      expect(await transport.exchange('a', wait), isNull);
      second = await transport.exchange('b', wait);
      await transport.close();
    }, (e, s) => errors.add(e));

    expect(errors, isEmpty);
    expect(second, 'ok');
    expect(sockets, hasLength(2));
  });

  test(
    'a send that throws, and a bind that fails, cost an attempt and nothing more',
    () async {
      var binds = 0;
      final transport = UdpEnrollTransport(
        bind: () async {
          binds++;
          if (binds == 1) {
            throw const SocketException('no interface yet');
          }
          if (binds == 2) {
            return _ThrowingSend();
          }
          return FakeSocket(onSend: (self, data) => self.deliver('fine'));
        },
      );

      expect(await transport.exchange('1', wait), isNull);
      expect(await transport.exchange('2', wait), isNull);
      expect(await transport.exchange('3', wait), 'fine');
      expect(binds, 3);
      expect(transport.socketErrors, 2);
      await transport.close();
    },
  );

  test(
    'every attempt takes the whole wait, so a failing socket cannot make the caller spin',
    () async {
      final transport = UdpEnrollTransport(
        bind: () async => throw const SocketException('nope'),
      );
      final started = DateTime.now();
      for (var i = 0; i < 3; i++) {
        expect(await transport.exchange('x', wait), isNull);
      }
      expect(
        DateTime.now().difference(started),
        greaterThanOrEqualTo(wait * 3 - const Duration(milliseconds: 40)),
      );
      await transport.close();
    },
  );

  test('a reply that arrives late is not mistaken for the next one', () async {
    late FakeSocket socket;
    final transport = UdpEnrollTransport(
      bind: () async => socket = FakeSocket(),
    );
    final first = transport.exchange('a', wait);
    expect(await first, isNull);
    socket.deliver('late');
    // Nobody was waiting: the late datagram is dropped, not queued for later.
    expect(await transport.exchange('b', wait), isNull);
    await transport.close();
  });

  test('after close, nothing is sent and nothing hangs', () async {
    final socket = FakeSocket();
    final transport = UdpEnrollTransport(bind: () async => socket);
    await transport.exchange('a', wait);
    await transport.close();
    expect(socket.closed, isTrue);
    expect(await transport.exchange('b', wait), isNull);
    expect(socket.sent, ['a']);
  });
  group('the data path probe on the same failure', () {
    // A DNS answer to whatever question was asked: same id, response bit set.
    List<int> answerTo(List<int> query) => List<int>.of(query)..[2] |= 0x80;

    test(
      'survives "host unreachable" and asks again from a fresh socket',
      () async {
        final errors = <Object>[];
        final sockets = <FakeSocket>[];
        bool? result;

        await runZonedGuarded(() async {
          final probe = UdpDnsProbe(
            bind: () async {
              final index = sockets.length;
              final socket = FakeSocket(
                onSend: (self, data) {
                  if (index == 0) {
                    self.failNextReceive(hostUnreachable());
                  } else {
                    self.deliverBytes(answerTo(data));
                  }
                },
              );
              sockets.add(socket);
              return socket;
            },
          );
          result = await probe.resolverAnswers(
            '198.18.0.1',
            const Duration(seconds: 5),
          );
        }, (e, s) => errors.add(e));

        expect(errors, isEmpty, reason: 'unhandled: $errors');
        expect(result, isTrue);
        expect(sockets.length, greaterThanOrEqualTo(2));
        expect(sockets.first.closed, isTrue);
      },
    );

    test(
      'says no, without hanging or throwing, when nothing ever works',
      () async {
        final errors = <Object>[];
        bool? result;
        await runZonedGuarded(() async {
          final probe = UdpDnsProbe(
            bind: () async => throw const SocketException('no interface'),
          );
          result = await probe.resolverAnswers(
            '198.18.0.1',
            const Duration(milliseconds: 2500),
          );
        }, (e, s) => errors.add(e));
        expect(errors, isEmpty);
        expect(result, isFalse);
      },
    );

    test('closes every socket it opened', () async {
      final sockets = <FakeSocket>[];
      final probe = UdpDnsProbe(
        bind: () async {
          final s = FakeSocket();
          sockets.add(s);
          return s;
        },
      );
      expect(
        await probe.resolverAnswers(
          '198.18.0.1',
          const Duration(milliseconds: 1500),
        ),
        isFalse,
      );
      expect(sockets, isNotEmpty);
      expect(sockets.every((s) => s.closed), isTrue);
    });
  });
}

class _ThrowingSend extends FakeSocket {
  @override
  int send(List<int> buffer, InternetAddress address, int port) =>
      throw hostUnreachable();
}
