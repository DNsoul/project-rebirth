import 'controller.dart';

/// How long to wait before each new try after a failed one. The network is often
/// not up yet when the system has only just signed someone in: the adapter is
/// still getting its address, the Wi-Fi has not joined. A minute and a half of
/// trying in all covers that without trying for ever at a machine that is
/// offline.
const logonRetryWaits = [
  Duration(seconds: 10),
  Duration(seconds: 20),
  Duration(seconds: 30),
  Duration(seconds: 30),
];

/// Connects for a program that was started with the system, and tries again if
/// the first try fails.
///
/// It gives way to the person at once. Only a failure is tried again: a
/// connection that came up, or that was called off ("cancel" leaves the screen
/// at "disconnected", not at an error), ends it, and so does anyone having
/// started a connection of their own while it was waiting.
Future<void> connectAtLogon(
  ConnectionView view, {
  List<Duration> waits = logonRetryWaits,
  Future<void> Function(Duration) sleep = _sleep,
  void Function(String line)? log,
}) async {
  if (!_free(view.status.phase)) {
    return;
  }
  for (var attempt = 0; ; attempt++) {
    await view.connect();
    final phase = view.status.phase;
    if (phase != Phase.failed) {
      return; // connected, or called off by a person
    }
    if (attempt >= waits.length) {
      log?.call('autostart: giving up after ${attempt + 1} tries');
      return;
    }
    log?.call(
      'autostart: not connected (${view.status.message}), '
      'trying again in ${waits[attempt].inSeconds}s',
    );
    await sleep(waits[attempt]);
    // Someone has taken over while this was waiting.
    if (view.status.phase != Phase.failed) {
      return;
    }
  }
}

bool _free(Phase p) => p == Phase.idle || p == Phase.failed;

Future<void> _sleep(Duration d) => Future<void>.delayed(d);
