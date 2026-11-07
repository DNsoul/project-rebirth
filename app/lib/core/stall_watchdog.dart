import 'dart:async';

/// Notices when the program's own thread has been stuck, and writes down for how
/// long.
///
/// A window that does not answer for a few seconds is hard to look into after
/// the fact. A timer that wants to fire every [interval] shows the stall as a
/// longer gap between two firings, and the gap goes to the log with the time it
/// happened at, so it can be put next to what the program was doing then.
class StallWatchdog {
  StallWatchdog({
    required this.log,
    this.interval = const Duration(milliseconds: 100),
    this.threshold = const Duration(milliseconds: 400),
  });

  final void Function(String line) log;
  final Duration interval;

  /// How much longer than expected a gap has to be to be worth a line.
  final Duration threshold;

  final _clock = Stopwatch();
  Timer? _timer;
  int _last = 0;

  /// Stalls seen so far, for tests.
  int stalls = 0;

  void start() {
    _clock.start();
    _last = _clock.elapsedMilliseconds;
    _timer = Timer.periodic(interval, (_) {
      final now = _clock.elapsedMilliseconds;
      final late = now - _last - interval.inMilliseconds;
      _last = now;
      if (late >= threshold.inMilliseconds) {
        stalls++;
        log('ui: the program was stuck for $late ms');
      }
    });
  }

  void stop() {
    _timer?.cancel();
    _timer = null;
    _clock.stop();
  }
}
