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

  /// Unix seconds of the last handshake, 0 when there has not been one, and -1
  /// for a handle that is not running.
  int latestHandshake(int handle);

  /// The descriptors of the client's UDP sockets, or -1.
  int socketV4(int handle);
  int socketV6(int handle);

  String lastError();
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
  }

  final DynamicLibrary _lib;
  late final int Function(Pointer<Utf8>, int) _turnOn;
  late final void Function(int) _turnOff;
  late final int Function(int) _handshake;
  late final int Function(int) _socket4;
  late final int Function(int) _socket6;
  late final Pointer<Utf8> Function() _lastError;
  late final Pointer<Utf8> Function() _version;
  late final void Function(Pointer<Utf8>) _free;

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
  int latestHandshake(int handle) => _handshake(handle);

  @override
  int socketV4(int handle) => _socket4(handle);

  @override
  int socketV6(int handle) => _socket6(handle);

  @override
  String lastError() => _take(_lastError());
}
