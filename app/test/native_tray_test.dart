import 'dart:async';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/stall_watchdog.dart';
import 'package:awg_client/platform/windows/tray.dart';
import 'package:awg_client/platform/windows/tray_state.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

/// A view that can be changed from the test, and records what was asked of it.
class FakeView implements ConnectionView {
  final _changes = StreamController<AppStatus>.broadcast();
  AppStatus _status = const AppStatus(Phase.idle);
  final asked = <String>[];

  void set(AppStatus s) {
    _status = s;
    _changes.add(s);
  }

  @override
  AppStatus get status => _status;

  @override
  Stream<AppStatus> get statusStream => _changes.stream;

  @override
  Future<void> connect() async => asked.add('connect');

  @override
  Future<void> disconnect() async => asked.add('disconnect');

  @override
  Future<void> cancel() async => asked.add('cancel');
}

final since = DateTime.utc(2026, 11, 7, 12);

Future<void> settle() async {
  for (var i = 0; i < 5; i++) {
    await Future<void>.delayed(Duration.zero);
  }
}

void main() {
  final binding = TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel(NativeTray.channelName);

  late FakeView view;
  late List<MethodCall> calls;
  late List<String> logs;
  late int quits;
  late NativeTray tray;

  /// What the runner's native side does when a menu item is chosen.
  Future<void> fromNative(String command) async {
    await binding.defaultBinaryMessenger.handlePlatformMessage(
      NativeTray.channelName,
      const StandardMethodCodec().encodeMethodCall(
        MethodCall('command', command),
      ),
      (_) {},
    );
    await settle();
  }

  List<MethodCall> updates() =>
      calls.where((c) => c.method == 'update').toList();

  setUp(() async {
    view = FakeView();
    calls = [];
    logs = [];
    quits = 0;
    binding.defaultBinaryMessenger.setMockMethodCallHandler(channel, (
      call,
    ) async {
      calls.add(call);
      return null;
    });
    tray = NativeTray(view: view, onQuit: () async => quits++, log: logs.add);
    await tray.init();
    await settle();
  });

  tearDown(() async {
    await tray.dispose();
    binding.defaultBinaryMessenger.setMockMethodCallHandler(channel, null);
  });

  group('what the native side is told', () {
    test('the state at the start, in full, at once', () {
      expect(updates(), hasLength(1));
      final a = updates().single.arguments as Map;
      expect(a['icon'], TrayIcon.idle.index);
      expect(a['tooltip'], 'Rebirth: отключено');
      expect(a['status'], 'Отключено');
      expect(a['toggleLabel'], 'Подключить');
      expect(a['toggleEnabled'], isTrue);
      expect(a['showLabel'], NativeTray.showLabel);
      expect(a['quitLabel'], NativeTray.quitLabel);
    });

    test('a change of state, when there is one', () async {
      view.set(const AppStatus(Phase.connecting, message: 'Готовлюсь…'));
      await settle();
      expect(updates(), hasLength(2));
      final a = updates().last.arguments as Map;
      expect(a['icon'], TrayIcon.busy.index);
      expect(a['toggleLabel'], 'Отменить');
    });

    test(
      'and only then: a changed progress message is not a new state',
      () async {
        view.set(const AppStatus(Phase.connecting, message: 'Готовлюсь…'));
        view.set(
          const AppStatus(Phase.connecting, message: 'Читаю настройки…'),
        );
        view.set(
          const AppStatus(Phase.connecting, message: 'Поднимаю туннель…'),
        );
        await settle();
        expect(updates(), hasLength(2), reason: 'idle, then connecting once');
      },
    );

    test('and not for the uptime ticking', () async {
      view.set(AppStatus(Phase.connected, region: 'waw', since: since));
      await settle();
      view.set(
        AppStatus(
          Phase.connected,
          region: 'waw',
          since: since.add(const Duration(hours: 1)),
        ),
      );
      await settle();
      expect(updates(), hasLength(2));
    });

    test('a place, and a failure with its reason on hover', () async {
      view.set(AppStatus(Phase.connected, region: 'sto', since: since));
      await settle();
      expect(
        (updates().last.arguments as Map)['status'],
        contains('Стокгольм'),
      );
      view.set(const AppStatus(Phase.failed, message: 'Нет связи'));
      await settle();
      final a = updates().last.arguments as Map;
      expect(a['icon'], TrayIcon.error.index);
      expect(a['tooltip'], 'Rebirth: Нет связи');
    });
  });

  group('what a choice in the menu does', () {
    test('"connect" when disconnected, and again after a failure', () async {
      await fromNative('toggle');
      view.set(const AppStatus(Phase.failed, message: 'x'));
      await settle();
      await fromNative('toggle');
      expect(view.asked, ['connect', 'connect']);
    });

    test('"disconnect" when connected', () async {
      view.set(AppStatus(Phase.connected, region: 'waw', since: since));
      await settle();
      await fromNative('toggle');
      expect(view.asked, ['disconnect']);
    });

    test('"cancel" while connecting or moving', () async {
      view.set(const AppStatus(Phase.connecting));
      await settle();
      await fromNative('toggle');
      view.set(const AppStatus(Phase.switching, region: 'ams'));
      await settle();
      await fromNative('toggle');
      expect(view.asked, ['cancel', 'cancel']);
    });

    test('nothing while the tunnel is coming down', () async {
      view.set(const AppStatus(Phase.disconnecting));
      await settle();
      await fromNative('toggle');
      expect(view.asked, isEmpty);
    });

    test('"quit" ends the app, and touches nothing else', () async {
      await fromNative('quit');
      expect(quits, 1);
      expect(view.asked, isEmpty);
    });

    test('something it does not know is ignored', () async {
      await fromNative('launch-the-missiles');
      expect(quits, 0);
      expect(view.asked, isEmpty);
    });
  });

  group('leaving', () {
    test('takes the icon away, and stops listening', () async {
      await tray.dispose();
      expect(calls.where((c) => c.method == 'remove'), hasLength(1));
      final before = updates().length;
      view.set(const AppStatus(Phase.connecting));
      await settle();
      expect(updates(), hasLength(before));
    });

    test('an icon that cannot be updated does not take the app down', () async {
      binding.defaultBinaryMessenger.setMockMethodCallHandler(channel, (
        call,
      ) async {
        throw PlatformException(code: 'gone', message: 'no notification area');
      });
      view.set(const AppStatus(Phase.connecting));
      await settle();
      expect(logs, contains(startsWith('tray: ')));
      expect(view.status.phase, Phase.connecting);
    });
  });

  group('the menu item that connects, disconnects or cancels', () {
    test('depends on what the app is doing', () {
      expect(trayToggleAction(const AppStatus(Phase.idle)), TrayAction.connect);
      expect(
        trayToggleAction(const AppStatus(Phase.failed)),
        TrayAction.connect,
      );
      expect(
        trayToggleAction(const AppStatus(Phase.connected)),
        TrayAction.disconnect,
      );
      expect(
        trayToggleAction(const AppStatus(Phase.connecting)),
        TrayAction.cancel,
      );
      expect(
        trayToggleAction(const AppStatus(Phase.switching)),
        TrayAction.cancel,
      );
      expect(
        trayToggleAction(const AppStatus(Phase.disconnecting)),
        TrayAction.none,
      );
    });

    test('is greyed out exactly when it does nothing', () {
      for (final p in Phase.values) {
        final s = AppStatus(p);
        expect(
          trayStateFor(s).toggleEnabled,
          trayToggleAction(s) != TrayAction.none,
          reason: '$p',
        );
      }
    });
  });

  group('the watchdog for a window that stops answering', () {
    /// Holds the program's one thread, as a blocking call would.
    void block(int milliseconds) {
      final sw = Stopwatch()..start();
      while (sw.elapsedMilliseconds < milliseconds) {}
    }

    test('writes down how long the thread was stuck', () async {
      final lines = <String>[];
      final dog = StallWatchdog(
        log: lines.add,
        interval: const Duration(milliseconds: 10),
        threshold: const Duration(milliseconds: 100),
      )..start();
      await Future<void>.delayed(const Duration(milliseconds: 60));
      block(350);
      await Future<void>.delayed(const Duration(milliseconds: 60));
      dog.stop();

      expect(dog.stalls, greaterThanOrEqualTo(1));
      final m = RegExp(r'stuck for (\d+) ms').firstMatch(lines.join('\n'));
      expect(m, isNotNull, reason: lines.toString());
      expect(int.parse(m!.group(1)!), greaterThanOrEqualTo(250));
    });

    test('says nothing while the program keeps up', () async {
      final lines = <String>[];
      final dog = StallWatchdog(
        log: lines.add,
        interval: const Duration(milliseconds: 10),
        threshold: const Duration(milliseconds: 300),
      )..start();
      await Future<void>.delayed(const Duration(milliseconds: 250));
      dog.stop();
      expect(lines, isEmpty);
      expect(dog.stalls, 0);
    });

    test('says nothing once stopped', () async {
      final lines = <String>[];
      final dog = StallWatchdog(
        log: lines.add,
        interval: const Duration(milliseconds: 10),
        threshold: const Duration(milliseconds: 100),
      )..start();
      await Future<void>.delayed(const Duration(milliseconds: 40));
      dog.stop();
      block(300);
      await Future<void>.delayed(const Duration(milliseconds: 40));
      expect(lines, isEmpty);
    });
  });
}
