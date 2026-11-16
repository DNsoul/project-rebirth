import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import 'dart:typed_data';

import 'platform.dart';

/// Asks the resolver on the other end of the tunnel a question and sees whether
/// the answer comes back.
///
/// This is the proof that matters. A handshake only shows that the server knows
/// the device's key; the server forwards a device's packets only from the
/// address it has on record for it, so a device with the right key and the wrong
/// address handshakes happily and then carries nothing. An answer from the
/// resolver means the whole path works.
///
/// A question goes out every second from a socket of its own. Right after the
/// tunnel comes up Windows answers a send with "host unreachable" until the route
/// is in place, and a socket that has said so is not to be trusted again (the
/// registration socket died of exactly this on the first real run): it is closed,
/// and the next question starts clean.
///
/// The others stay open until the end, and an answer on any of them counts. The
/// resolver looks a name up abroad before it answers, and an answer that took
/// longer than a second used to arrive at a socket that had already been closed.
class UdpDnsProbe implements DataPathProbe {
  UdpDnsProbe({
    this.name = 'one.one.one.one',
    this.port = 53,
    DatagramBinder? bind,
  }) : _bind = bind ?? bindAnyDatagram;

  final String name;

  /// 53 everywhere but in tests.
  final int port;

  final DatagramBinder _bind;

  String _report = '';

  @override
  String get lastReport => _report;

  @override
  Future<bool> resolverAnswers(String server, Duration within) async {
    _report = '';
    final target = InternetAddress.tryParse(server);
    if (target == null) {
      _report = '"$server" is not an address';
      return false;
    }
    final id = Random.secure().nextInt(0x10000);
    final query = buildQuery(id, name);
    final answered = Completer<void>();
    final open = <RawDatagramSocket>[];
    final subscriptions = <StreamSubscription<RawSocketEvent>>[];
    var asked = 0;
    var errors = 0;
    String? lastError;
    void failed(Object e) {
      errors++;
      lastError = _short(e);
    }

    final deadline = DateTime.now().add(within);
    try {
      while (!answered.isCompleted && DateTime.now().isBefore(deadline)) {
        asked++;
        try {
          final s = await _bind();
          open.add(s);
          subscriptions.add(
            s.listen(
              (event) {
                if (event != RawSocketEvent.read) {
                  return;
                }
                try {
                  final datagram = s.receive();
                  if (datagram != null &&
                      isAnswerTo(id, datagram.data) &&
                      !answered.isCompleted) {
                    answered.complete();
                  }
                } on Object catch (e) {
                  failed(e);
                  _close(s);
                }
              },
              onError: (Object e) {
                failed(e);
                _close(s);
              },
              cancelOnError: true,
            ),
          );
          s.send(query, target, port);
        } on Object catch (e) {
          failed(e);
        }
        // The next question in a second, unless the answer comes first. Always a
        // wait, so that failing quickly cannot turn this into a spin.
        final left = deadline.difference(DateTime.now());
        final wait = left < const Duration(seconds: 1)
            ? left
            : const Duration(seconds: 1);
        if (wait > Duration.zero) {
          await Future.any([answered.future, Future<void>.delayed(wait)]);
        }
      }
      if (answered.isCompleted) {
        return true;
      }
      _report =
          'asked $asked times, $errors socket errors'
          '${lastError == null ? '' : ' (last: $lastError)'}, no answer';
      return false;
    } finally {
      for (final sub in subscriptions) {
        await sub.cancel();
      }
      for (final s in open) {
        _close(s);
      }
    }
  }

  static void _close(RawDatagramSocket s) {
    try {
      s.close();
    } on Object {
      // Already closed.
    }
  }

  /// An error in a few words: the system's own message is long and, on a
  /// Russian Windows, in Russian; the number is what can be looked up.
  static String _short(Object e) {
    if (e is SocketException) {
      final code = e.osError?.errorCode;
      return code == null ? e.message : 'error $code';
    }
    return e.runtimeType.toString();
  }
}

/// An A query for [name], in DNS wire format.
Uint8List buildQuery(int id, String name) {
  final out = BytesBuilder()
    ..add([id >> 8, id & 0xff]) // id
    ..add([0x01, 0x00]) // standard query, recursion desired
    ..add([0x00, 0x01]) // one question
    ..add([0, 0, 0, 0, 0, 0]); // no other records
  for (final label in name.split('.')) {
    final bytes = utf8.encode(label);
    out
      ..addByte(bytes.length)
      ..add(bytes);
  }
  out
    ..addByte(0)
    ..add([0x00, 0x01]) // type A
    ..add([0x00, 0x01]); // class IN
  return out.toBytes();
}

/// Whether [data] is a DNS response carrying the same [id], whatever it says:
/// that it came back at all is the point.
bool isAnswerTo(int id, List<int> data) {
  if (data.length < 12) return false;
  final gotId = (data[0] << 8) | data[1];
  final isResponse = data[2] & 0x80 != 0;
  return gotId == id && isResponse;
}
