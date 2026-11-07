import 'dart:async';

/// Thrown by an operation that was told to stop. It is not a failure and is
/// never shown to the user: the person who asked for the stop already knows.
class Cancelled implements Exception {
  const Cancelled();

  @override
  String toString() => 'cancelled';
}

/// A way to tell an operation that has many slow steps to stop.
///
/// Connecting takes many seconds and is made of waits: for the settings, for the
/// server, for the tunnel. If the person presses "disconnect" or "cancel" in the
/// middle, the app must answer at once, not after the step in progress. Each step
/// looks at the token before it starts and while it waits.
class CancelToken {
  bool _cancelled = false;
  final _signal = Completer<void>();

  bool get isCancelled => _cancelled;

  /// Completes when the token is cancelled.
  Future<void> get whenCancelled => _signal.future;

  void cancel() {
    if (!_cancelled) {
      _cancelled = true;
      _signal.complete();
    }
  }

  /// Stops the operation here if it has been told to.
  void check() {
    if (_cancelled) {
      throw const Cancelled();
    }
  }

  /// Waits for [work], but stops waiting the moment the token is cancelled. The
  /// work itself may carry on in the background until it ends by itself (a web
  /// request cannot be recalled); what matters is that the caller no longer waits
  /// for it.
  Future<T> guard<T>(Future<T> work) {
    if (_cancelled) {
      // Not interested in how it ends.
      unawaited(work.then<void>((_) {}, onError: (Object _) {}));
      throw const Cancelled();
    }
    return Future.any<T>([
      work,
      _signal.future.then<T>((_) => throw const Cancelled()),
    ]);
  }
}
