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
/// Each question goes out from a socket of its own, which is thrown away after
/// it. Right after the tunnel comes up Windows answers a send with "host
/// unreachable" until the route is in place, and a socket that has said so is not
/// to be trusted again (the registration socket died of exactly this on the first
/// real run). A question that fails for that reason costs a second and the next
/// one starts clean.
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

  @override
  Future<bool> resolverAnswers(String server, Duration within) async {
    final target = InternetAddress.tryParse(server);
    if (target == null) {
      return false;
    }
    final id = Random.secure().nextInt(0x10000);
    final query = buildQuery(id, name);

    final deadline = DateTime.now().add(within);
    while (DateTime.now().isBefore(deadline)) {
      if (await _askOnce(target, query, id, const Duration(seconds: 1))) {
        return true;
      }
    }
    return false;
  }

  /// One question from a fresh socket. Always takes the whole [wait] unless it is
  /// answered, so that failing quickly cannot turn the caller's loop into a spin.
  Future<bool> _askOnce(
    InternetAddress target,
    Uint8List query,
    int id,
    Duration wait,
  ) async {
    final answered = Completer<bool>();
    final timer = Timer(wait, () {
      if (!answered.isCompleted) {
        answered.complete(false);
      }
    });
    RawDatagramSocket? socket;
    StreamSubscription<RawSocketEvent>? subscription;
    try {
      socket = await _bind();
      final s = socket;
      subscription = s.listen(
        (event) {
          if (event != RawSocketEvent.read) {
            return;
          }
          try {
            final datagram = s.receive();
            if (datagram != null &&
                isAnswerTo(id, datagram.data) &&
                !answered.isCompleted) {
              answered.complete(true);
            }
          } on Object {
            // The socket is finished with; the attempt just times out.
          }
        },
        onError: (Object _) {},
        cancelOnError: true,
      );
      s.send(query, target, port);
    } on Object {
      // Could not open or send; the attempt just times out.
    }
    try {
      return await answered.future;
    } finally {
      timer.cancel();
      await subscription?.cancel();
      try {
        socket?.close();
      } on Object {
        // Already closed.
      }
    }
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
