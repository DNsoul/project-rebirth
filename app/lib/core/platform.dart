/// What the app needs from the machine it runs on. The logic in this folder
/// knows nothing about Windows or Android: each platform gives it these, and
/// the tests give it fakes.
library;

import 'dart:io';

/// Opens a datagram socket. Replaced in tests, to make one that misbehaves the
/// way a real one did.
typedef DatagramBinder = Future<RawDatagramSocket> Function();

/// A socket not bound to anything in particular: the system picks the interface
/// and the source address when a datagram is sent.
Future<RawDatagramSocket> bindAnyDatagram() =>
    RawDatagramSocket.bind(InternetAddress.anyIPv4, 0);

/// A tunnel could not be brought up or managed. The message is for the user.
class TunnelException implements Exception {
  TunnelException(this.message);

  final String message;

  @override
  String toString() => message;
}

/// Brings the tunnel up and down.
abstract class TunnelDriver {
  /// Why the tunnel cannot be managed from here, in words for the user, or null
  /// when it can. Checked before anything is attempted.
  Future<String?> preflight();

  /// Installs the tunnel called [name] from [conf] and starts it. Replaces a
  /// tunnel of the same name.
  Future<void> install({required String name, required String conf});

  /// Stops and removes the tunnel. Harmless if there is none.
  Future<void> uninstall(String name);

  /// When the tunnel last completed a handshake with the server, or null if it
  /// has not yet. A handshake is the proof that the server accepted the device.
  Future<DateTime?> latestHandshake(String name);

  /// The names of the tunnels this app has installed and not removed. After a
  /// crash these are what has to be cleaned up.
  Future<List<String>> installed();
}

/// Makes the machine ask the server's resolver, and only it, for names. Without
/// this, the system also asks whatever resolver the ordinary connection offers,
/// and which sites go through the tunnel stops being the server's decision.
abstract class DnsGuard {
  /// Sends every query to [server].
  Future<void> enable(String server);

  /// Puts the machine's own resolution back. Harmless if it was not enabled.
  Future<void> disable();
}

/// Checks that traffic really passes through the tunnel.
abstract class DataPathProbe {
  /// Asks the resolver at [server] a question through the tunnel and waits up to
  /// [within] for an answer. A handshake alone proves less than it seems to:
  /// see the implementation for why.
  Future<bool> resolverAnswers(String server, Duration within);
}

/// Something that identifies this installation of the system and does not
/// change when the app is reinstalled.
abstract class MachineIdSource {
  Future<String> machineId();
}

/// Sends registration datagrams through the enrollment tunnel.
abstract class EnrollTransport {
  /// Sends [datagram] to the server's registration service and waits up to
  /// [wait] for one datagram back. Null means nothing came.
  Future<String?> exchange(String datagram, Duration wait);

  /// What happened, in a line for the log when it did not work out.
  String describe() => '';

  Future<void> close();
}

typedef EnrollTransportFactory = EnrollTransport Function();
