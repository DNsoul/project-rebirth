import 'dart:async';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/logon_connect.dart';
import 'package:flutter_test/flutter_test.dart';

/// A connection whose attempts end as the test says, one after another.
class _Scripted implements ConnectionView {
  _Scripted(this.results);

  /// What each call to connect() ends with; the last one repeats.
  final List<AppStatus> results;
  AppStatus _status = const AppStatus(Phase.idle);
  int connects = 0;
  void Function()? duringConnect;

  @override
  AppStatus get status => _status;

  @override
  Stream<AppStatus> get statusStream => const Stream.empty();

  void set(AppStatus s) => _status = s;

  @override
  Future<void> connect() async {
    connects++;
    duringConnect?.call();
    final i = connects <= results.length ? connects - 1 : results.length - 1;
    _status = results[i];
  }

  @override
  Future<void> disconnect() async {}

  @override
  Future<void> cancel() async {}
}

const _failed = AppStatus(Phase.failed, message: 'Нет связи с GitHub.');
const _connected = AppStatus(Phase.connected, region: 'sto');

void main() {
  final slept = <Duration>[];
  Future<void> sleep(Duration d) async => slept.add(d);

  setUp(slept.clear);

  test('connects once when that works', () async {
    final view = _Scripted([_connected]);
    await connectAtLogon(view, sleep: sleep);
    expect(view.connects, 1);
    expect(slept, isEmpty);
  });

  test('tries again after a failure, waiting longer each time', () async {
    final view = _Scripted([_failed, _failed, _connected]);
    await connectAtLogon(view, sleep: sleep);
    expect(view.connects, 3);
    expect(slept, logonRetryWaits.take(2).toList());
    expect(view.status.phase, Phase.connected);
  });

  test('gives up after the last wait, not before', () async {
    final view = _Scripted([_failed]);
    final lines = <String>[];
    await connectAtLogon(view, sleep: sleep, log: lines.add);
    expect(view.connects, logonRetryWaits.length + 1);
    expect(slept, logonRetryWaits);
    expect(lines.last, contains('giving up'));
    expect(view.status.phase, Phase.failed, reason: 'the screen says why');
  });

  test('a connection called off is not tried again', () async {
    // "Cancel" leaves the screen at "disconnected", not at an error.
    final view = _Scripted([const AppStatus(Phase.idle)]);
    await connectAtLogon(view, sleep: sleep);
    expect(view.connects, 1);
    expect(slept, isEmpty);
  });

  test('does nothing if a connection is already on its way or up', () async {
    for (final phase in [Phase.connecting, Phase.connected, Phase.switching]) {
      final view = _Scripted([_connected])..set(AppStatus(phase));
      await connectAtLogon(view, sleep: sleep);
      expect(view.connects, 0, reason: '$phase');
    }
  });

  test('gives way to a person who connects while it is waiting', () async {
    final view = _Scripted([_failed]);
    Future<void> personPressesConnect(Duration _) async {
      view.set(const AppStatus(Phase.connecting));
    }

    await connectAtLogon(view, sleep: personPressesConnect);
    expect(view.connects, 1, reason: 'the person is now in charge');
  });
}
