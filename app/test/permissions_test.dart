import 'package:awg_client/platform/android/permissions.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

import 'android_test.dart' show FakeBridge;

void main() {
  test(
    'asks about everything the app needs, one thing after another',
    () async {
      final bridge = FakeBridge();
      await askForPermissions(bridge);
      expect(bridge.calls, ['notifications', 'battery', 'install']);
    },
  );

  test('says what the person answered to each', () async {
    final bridge = FakeBridge();
    bridge.answers['notifications'] = 'denied';
    bridge.answers['battery'] = 'granted';
    bridge.answers['install'] = 'denied';
    final log = <String>[];
    await askForPermissions(bridge, log: log.add);
    expect(log, [
      'permissions: notifications denied',
      'permissions: battery granted',
      'permissions: install denied',
    ]);
  });

  test(
    'asks about nothing else: the VPN comes with the start-up check',
    () async {
      final bridge = FakeBridge();
      await askForPermissions(bridge);
      expect(bridge.calls, isNot(contains('prepare')));
    },
  );

  test(
    'a refusal is not an error: the next question is asked all the same',
    () async {
      for (final answer in ['denied', 'settings']) {
        final bridge = FakeBridge();
        bridge.answers['notifications'] = answer;
        bridge.answers['battery'] = answer;
        await askForPermissions(bridge);
        expect(bridge.calls, [
          'notifications',
          'battery',
          'install',
        ], reason: answer);
      }
    },
  );

  test(
    'a failure of one question is written down and the others still asked',
    () async {
      final bridge = FakeBridge();
      bridge.failures['battery'] = PlatformException(code: 'busy');
      final log = <String>[];
      await askForPermissions(bridge, log: log.add);
      expect(bridge.calls, ['notifications', 'battery', 'install']);
      expect(log[0], 'permissions: notifications granted');
      expect(log[1], allOf(contains('battery'), contains('busy')));
      expect(log[2], 'permissions: install granted');
    },
  );

  test(
    'does not ask the next question until the last has been answered',
    () async {
      // The system's dialogs would cover each other, and the later one would hide
      // the earlier from the person.
      final bridge = FakeBridge();
      await askForPermissions(bridge);
      final order = bridge.calls.toList();
      expect(
        order.indexOf('notifications'),
        lessThan(order.indexOf('battery')),
      );
      expect(order.indexOf('battery'), lessThan(order.indexOf('install')));
    },
  );
}
