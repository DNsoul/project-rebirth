import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'platform.dart';

/// Sends registration datagrams to the server's registration service through
/// the enrollment tunnel.
///
/// The socket is not bound to anything in particular: the enrollment tunnel
/// routes the one address it may reach, so the system sends the datagram through
/// it and the server sees it arrive from the enrollment peer's own address.
///
/// The tunnel is reported as running a moment before its route to the server
/// exists, and until it does Windows answers a send with "host unreachable" (error
/// 1232). That error comes back from the socket itself, from `receive()` and
/// from the event stream, and a socket that has reported it is not to be trusted
/// again. So every error is caught, the socket is thrown away, and the next
/// attempt opens a fresh one. The first version let it escape as an unhandled
/// exception from inside the event handler; registration never read another
/// reply, and every attempt ended in a timeout.
class UdpEnrollTransport implements EnrollTransport {
  UdpEnrollTransport({
    this.host = '10.8.0.1',
    this.port = 7843,
    DatagramBinder? bind,
    this.log,
  }) : _bind = bind ?? bindAnyDatagram;

  final String host;
  final int port;
  final void Function(String line)? log;
  final DatagramBinder _bind;

  RawDatagramSocket? _socket;
  StreamSubscription<RawSocketEvent>? _subscription;
  final _replies = StreamController<String>.broadcast();
  bool _closed = false;

  /// Counted for the log: what happened, in case it did not work.
  int sent = 0;
  int replies = 0;
  int socketErrors = 0;
  String? lastError;

  void _note(Object error) {
    socketErrors++;
    lastError = '$error';
    log?.call('registration socket: $error');
  }

  /// Gives up a socket that has misbehaved. The next exchange makes a new one.
  void _drop(RawDatagramSocket socket) {
    if (identical(_socket, socket)) {
      _socket = null;
      _subscription?.cancel();
      _subscription = null;
    }
    try {
      socket.close();
    } on Object {
      // Already gone.
    }
  }

  Future<RawDatagramSocket?> _open() async {
    final existing = _socket;
    if (existing != null) {
      return existing;
    }
    final RawDatagramSocket socket;
    try {
      socket = await _bind();
    } on Object catch (e) {
      _note(e);
      return null;
    }
    _subscription = socket.listen(
      (event) {
        if (event != RawSocketEvent.read) {
          return;
        }
        try {
          final datagram = socket.receive();
          if (datagram != null && !_replies.isClosed) {
            replies++;
            _replies.add(utf8.decode(datagram.data, allowMalformed: true));
          }
        } on Object catch (e) {
          _note(e);
          _drop(socket);
        }
      },
      onError: (Object e) {
        _note(e);
        _drop(socket);
      },
      cancelOnError: true,
    );
    return _socket = socket;
  }

  @override
  Future<String?> exchange(String datagram, Duration wait) async {
    if (_closed) {
      return null;
    }
    final answer = Completer<String?>();
    final listener = _replies.stream.listen((reply) {
      if (!answer.isCompleted) {
        answer.complete(reply);
      }
    });
    // Always the full wait, even when something fails: if an error ended the
    // attempt at once, the caller would send again at once and spin.
    final timer = Timer(wait, () {
      if (!answer.isCompleted) {
        answer.complete(null);
      }
    });

    try {
      final socket = await _open();
      if (socket != null) {
        try {
          final n = socket.send(
            utf8.encode(datagram),
            InternetAddress(host),
            port,
          );
          if (n > 0) {
            sent++;
          }
        } on Object catch (e) {
          _note(e);
          _drop(socket);
        }
      }
      return await answer.future;
    } finally {
      timer.cancel();
      await listener.cancel();
    }
  }

  @override
  String describe() =>
      'sent $sent, replies $replies, socket errors $socketErrors'
      '${lastError == null ? '' : ', last: $lastError'}';

  @override
  Future<void> close() async {
    _closed = true;
    final socket = _socket;
    if (socket != null) {
      _drop(socket);
    }
    await _replies.close();
  }
}
