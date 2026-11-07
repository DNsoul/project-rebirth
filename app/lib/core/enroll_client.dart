import 'dart:async';
import 'dart:math';
import 'dart:typed_data';

import 'package:awg_proto/awg_proto.dart';

import 'cancel.dart';
import 'platform.dart';

/// The server said no.
class EnrollRefused implements Exception {
  EnrollRefused(this.reason);

  /// One of the `enroll*` reasons from awg_proto.
  final String reason;

  @override
  String toString() {
    switch (reason) {
      case enrollFull:
        return 'На сервере уже зарегистрировано столько устройств, сколько он принимает.';
      case enrollKeyInUse:
        return 'Этот ключ уже закреплён за другим устройством.';
      case enrollBusy:
        return 'Сервер занят регистрацией других устройств.';
      default:
        return 'Сервер отказал в регистрации ($reason).';
    }
  }
}

/// Registers this device on the server and returns the address it was given,
/// with prefix.
///
/// The datagram is sent again every second until an answer comes: the first
/// packets after a handshake can be lost, and a request that arrives twice does
/// no harm. A "busy" answer is waited out with a short pause and a fresh request.
/// Anything else the server says is final.
Future<String> registerDevice({
  required EnrollTransport transport,
  required Uint8List contentKey,
  required Uint8List deviceId,
  required Uint8List publicKey,
  String name = '',
  DateTime Function()? now,
  Duration timeout = const Duration(seconds: 45),
  Future<void> Function(Duration)? sleep,
  Random? random,
  CancelToken? cancel,
}) async {
  final clock = now ?? DateTime.now;
  final wait = sleep ?? (Duration d) => Future<void>.delayed(d);
  final rng = random ?? Random();
  final deadline = clock().add(timeout);

  while (true) {
    cancel?.check();
    final request = EnrollRequest.create(
      deviceId: deviceId,
      publicKey: publicKey,
      now: clock(),
      name: name,
    );
    final datagram = await request.seal(contentKey);

    EnrollResponse? response;
    while (response == null) {
      cancel?.check();
      if (!clock().isBefore(deadline)) {
        throw TimeoutException('Сервер не ответил на регистрацию.');
      }
      final raw = await transport.exchange(
        datagram,
        const Duration(seconds: 1),
      );
      if (raw == null) continue; // nothing yet; send it again
      try {
        response = await openEnrollResponse(contentKey, raw, request.nonce);
      } on ProtoException {
        // Not an answer to this request: a late one to an earlier send, or
        // something else on the wire. Keep listening.
        continue;
      }
    }

    if (response.granted) return response.address;
    if (response.error != enrollBusy) throw EnrollRefused(response.error);

    if (!clock().isBefore(deadline)) throw EnrollRefused(enrollBusy);
    final pause = wait(Duration(milliseconds: 1000 + rng.nextInt(3000)));
    await (cancel == null ? pause : cancel.guard(pause));
  }
}
