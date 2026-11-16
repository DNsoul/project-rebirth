import 'dart:ffi';

import 'package:ffi/ffi.dart';

/// The tunnel library (libawg.so, built from go/cmd/awglib), as Dart sees it.
/// The tests give the driver a fake of this; the app gives it [FfiAwgNative].
abstract class AwgNative {
  /// Which library this is, so a mismatch with the app is visible at once.
  String version();

  /// Starts a client from a configuration file's text on the TUN descriptor,
  /// which the library then owns. Returns a handle (1 or more) or a negative
  /// number, in which case [lastError] says why.
  int turnOn(String conf, int tunFd);

  /// Stops a client and closes its descriptor. Harmless for a handle that is not
  /// running.
  void turnOff(int handle);

  /// Stops every client the library is running and says how many there were.
  /// For tunnels that outlived the program that started them.
  int turnOffAll();

  /// For a change of network: opens the client's sockets afresh and starts a
  /// new handshake at once. 0 when it worked, a negative number when it did not
  /// ([lastError] says why). The sockets have new descriptors afterwards.
  int rebind(int handle);

  /// Unix seconds of the last handshake, 0 when there has not been one, and -1
  /// for a handle that is not running.
  int latestHandshake(int handle);

  /// The descriptors of the client's UDP sockets, or -1.
  int socketV4(int handle);
  int socketV6(int handle);

  /// Bytes received from the server and sent to it, or null for a handle that is
  /// not running (or a library too old to say).
  ({int rx, int tx})? traffic(int handle);

  String lastError();
}

/// The library's "rx tx". Null when it is not that.
({int rx, int tx})? parseTraffic(String text) {
  final parts = text.trim().split(' ');
  if (parts.length != 2) return null;
  final rx = int.tryParse(parts[0]), tx = int.tryParse(parts[1]);
  return rx == null || tx == null ? null : (rx: rx, tx: tx);
}

/// The real thing: calls into libawg.so, which sits next to the app's own
/// libraries.
class FfiAwgNative implements AwgNative {
  FfiAwgNative([DynamicLibrary? library])
    : _lib = library ?? DynamicLibrary.open('libawg.so') {
    _turnOn = _lib
        .lookupFunction<
          Int32 Function(Pointer<Utf8>, Int32),
          int Function(Pointer<Utf8>, int)
        >('awgTurnOn');
    _turnOff = _lib.lookupFunction<Void Function(Int32), void Function(int)>(
      'awgTurnOff',
    );
    _turnOffAll = _lib.lookupFunction<Int32 Function(), int Function()>(
      'awgTurnOffAll',
    );
    _rebind = _lib.lookupFunction<Int32 Function(Int32), int Function(int)>(
      'awgRebind',
    );
    _handshake = _lib.lookupFunction<Int64 Function(Int32), int Function(int)>(
      'awgLatestHandshake',
    );
    _socket4 = _lib.lookupFunction<Int32 Function(Int32), int Function(int)>(
      'awgSocketV4',
    );
    _socket6 = _lib.lookupFunction<Int32 Function(Int32), int Function(int)>(
      'awgSocketV6',
    );
    _lastError = _lib
        .lookupFunction<Pointer<Utf8> Function(), Pointer<Utf8> Function()>(
          'awgLastError',
        );
    _version = _lib
        .lookupFunction<Pointer<Utf8> Function(), Pointer<Utf8> Function()>(
          'awgVersion',
        );
    _free = _lib
        .lookupFunction<
          Void Function(Pointer<Utf8>),
          void Function(Pointer<Utf8>)
        >('awgFree');
    // Only for the journal: a library without it still runs tunnels.
    try {
      _traffic = _lib
          .lookupFunction<
            Pointer<Utf8> Function(Int32),
            Pointer<Utf8> Function(int)
          >('awgTraffic');
    } on ArgumentError {
      _traffic = null;
    }
  }

  final DynamicLibrary _lib;
  late final int Function(Pointer<Utf8>, int) _turnOn;
  late final void Function(int) _turnOff;
  late final int Function() _turnOffAll;
  late final int Function(int) _rebind;
  late final int Function(int) _handshake;
  late final int Function(int) _socket4;
  late final int Function(int) _socket6;
  late final Pointer<Utf8> Function() _lastError;
  late final Pointer<Utf8> Function() _version;
  late final void Function(Pointer<Utf8>) _free;
  late final Pointer<Utf8> Function(int)? _traffic;

  /// Text the library returned, copied and released.
  String _take(Pointer<Utf8> p) {
    try {
      return p.toDartString();
    } finally {
      _free(p);
    }
  }

  @override
  String version() => _take(_version());

  @override
  int turnOn(String conf, int tunFd) {
    final text = conf.toNativeUtf8();
    try {
      return _turnOn(text, tunFd);
    } finally {
      calloc.free(text);
    }
  }

  @override
  void turnOff(int handle) => _turnOff(handle);

  @override
  int turnOffAll() => _turnOffAll();

  @override
  int rebind(int handle) => _rebind(handle);

  @override
  int latestHandshake(int handle) => _handshake(handle);

  @override
  int socketV4(int handle) => _socket4(handle);

  @override
  int socketV6(int handle) => _socket6(handle);

  @override
  ({int rx, int tx})? traffic(int handle) {
    final f = _traffic;
    if (f == null) return null;
    final p = f(handle);
    if (p == nullptr) return null;
    return parseTraffic(_take(p));
  }

  @override
  String lastError() => _take(_lastError());
}
