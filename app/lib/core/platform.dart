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

  /// Whether installing a tunnel takes the way into the tunnels away from the
  /// others. On Android an app has one VPN interface, and opening one closes the
  /// one before: a tunnel that was running goes on talking to its server, but
  /// nothing on the phone reaches it any more. On Windows every tunnel has an
  /// adapter of its own.
  bool get oneAtATime;

  /// Stops and removes the tunnel. Harmless if there is none.
  Future<void> uninstall(String name);

  /// When the tunnel last completed a handshake with the server, or null if it
  /// has not yet. A handshake is the proof that the server accepted the device.
  Future<DateTime?> latestHandshake(String name);

  /// The names of the tunnels this app has installed and not removed. After a
  /// crash these are what has to be cleaned up.
  Future<List<String>> installed();

  /// The network the machine reaches the internet by has changed (Wi-Fi to mobile
  /// data, or back). Makes the tunnel find the server on the new path at once,
  /// rather than leaving it to its own timers. Harmless if there is no such
  /// tunnel, and a no-op where the system's own client already does this.
  Future<void> rebind(String name);

  /// Whether [rebind] makes a handshake at once, so that its answer can be
  /// waited for. True on Android, where Rebirth's own library does it; false on
  /// Windows, where [rebind] does nothing and the tunnel service is left to it.
  bool get rebindMakesHandshake;

  /// Fires when the interface the tunnel runs on was opened for a set of apps
  /// let past it that is no longer right: on Android, an app was installed or
  /// removed. Never on Windows, where every app goes the same way.
  Stream<void> get interfaceOutdated;

  /// Whether the interface should be opened again. Asked right before doing it,
  /// so that a change a newer interface already took in costs nothing.
  Future<bool> isInterfaceOutdated();

  /// What can be told about why the tunnel [name] carries nothing, in words for
  /// the journal: what it has received and sent, and, where the system can say,
  /// which way it sends traffic for [address]. Empty when nothing can be told.
  /// Asked only after something failed, so it may take a moment; never throws.
  Future<String> diagnose(String name, String address);
}

/// Bytes received and sent, in words for the journal.
String describeTraffic(int received, int sent) =>
    'received ${_bytes(received)}, sent ${_bytes(sent)}';

String _bytes(int n) => n < 10 * 1024
    ? '$n B'
    : n < 10 * 1024 * 1024
    ? '${n ~/ 1024} KB'
    : '${n ~/ (1024 * 1024)} MB';

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

  /// After a check that got no answer: what it went through (how many times it
  /// asked, what the sockets said), in words for the journal. Empty after an
  /// answer.
  String get lastReport;
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
