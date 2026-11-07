import 'dart:async';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/updater.dart';
import 'package:awg_client/ui/home_page.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

/// A screen's source that stays where it is told, and records what was pressed.
class StaticView implements ConnectionView {
  StaticView(this.status);

  @override
  final AppStatus status;
  final presses = <String>[];

  @override
  Stream<AppStatus> get statusStream => const Stream.empty();

  @override
  Future<void> connect() async => presses.add('connect');

  @override
  Future<void> disconnect() async => presses.add('disconnect');

  @override
  Future<void> cancel() async => presses.add('cancel');
}

/// A view that can change, to see the screen follow it.
class LiveView implements ConnectionView {
  final _controller = StreamController<AppStatus>.broadcast();
  AppStatus _status = const AppStatus(Phase.idle);
  final presses = <String>[];

  void set(AppStatus s) {
    _status = s;
    _controller.add(s);
  }

  @override
  AppStatus get status => _status;

  @override
  Stream<AppStatus> get statusStream => _controller.stream;

  @override
  Future<void> connect() async => presses.add('connect');

  @override
  Future<void> disconnect() async => presses.add('disconnect');

  @override
  Future<void> cancel() async => presses.add('cancel');
}

class FixedUpdates implements UpdateView {
  FixedUpdates(this.status);

  @override
  final UpdateStatus? status;
  int installs = 0;

  @override
  Stream<UpdateStatus?> get statusStream => const Stream.empty();

  @override
  Future<void> install() async => installs++;
}

UpdateStatus update(UpdatePhase phase, {String message = ''}) {
  final r = unsignedRelease();
  return UpdateStatus(
    phase,
    UpdateOffer(release: r, asset: r.asset(osWindows)!, mandatory: false),
    message: message,
  );
}

Widget app(ConnectionView view, {UpdateView? updates}) => MaterialApp(
  debugShowCheckedModeBanner: false,
  theme: appTheme(),
  // A fresh screen each time: a stream builder keeps what it last showed when the
  // widget above it is swapped for another of the same kind.
  home: HomePage(key: UniqueKey(), controller: view, updates: updates),
);

const buttonKey = ValueKey('power-button');

/// The window as the tests have it: [size] logical pixels.
void windowOf(WidgetTester tester, Size size) {
  tester.view.physicalSize = size;
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
}

void main() {
  const window = Size(324, 521);

  group('the screen', () {
    testWidgets('is one button and nothing written anywhere', (tester) async {
      windowOf(tester, window);
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
      expect(find.byKey(buttonKey), findsOneWidget);
      expect(find.byType(Text), findsNothing);
      expect(find.byType(RichText), findsNothing);
    });

    testWidgets('says nothing in any state, however much the status knows', (
      tester,
    ) async {
      windowOf(tester, window);
      final states = <AppStatus>[
        const AppStatus(Phase.connecting, message: 'Поднимаю туннель…'),
        AppStatus(
          Phase.connected,
          region: 'waw',
          since: DateTime.now().subtract(const Duration(minutes: 12)),
        ),
        AppStatus(Phase.connected, region: 'sto', lostContact: true),
        const AppStatus(
          Phase.switching,
          message: 'Перехожу на новый сервер…',
          region: 'sto',
        ),
        const AppStatus(Phase.disconnecting, message: 'Отключаю туннель…'),
      ];
      for (final s in states) {
        await tester.pumpWidget(app(StaticView(s)));
        expect(find.byType(Text), findsNothing, reason: '${s.phase}');
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('has nothing to scroll, whatever the size of the window', (
      tester,
    ) async {
      for (final size in const [
        Size(324, 521),
        Size(200, 300),
        Size(800, 300),
        Size(1280, 720),
        Size(360, 640),
      ]) {
        windowOf(tester, size);
        await tester.pumpWidget(
          app(
            StaticView(
              const AppStatus(
                Phase.failed,
                message:
                    'Сервер не ответил на регистрацию. Проверьте интернет и повторите.',
              ),
            ),
            updates: FixedUpdates(update(UpdatePhase.available)),
          ),
        );
        expect(find.byType(Scrollable), findsNothing, reason: '$size');
        expect(tester.takeException(), isNull, reason: '$size');
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('keeps the button exactly in the middle of the window', (
      tester,
    ) async {
      for (final size in const [
        Size(324, 521),
        Size(800, 300),
        Size(360, 780),
      ]) {
        windowOf(tester, size);
        await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
        final centre = tester.getCenter(find.byKey(buttonKey));
        expect(centre.dx, closeTo(size.width / 2, 0.5), reason: '$size');
        expect(centre.dy, closeTo(size.height / 2, 0.5), reason: '$size');
      }
    });

    testWidgets('the button does not move when an error or an update appears', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
      final before = tester.getCenter(find.byKey(buttonKey));
      final sizeBefore = tester.getSize(find.byKey(buttonKey));

      await tester.pumpWidget(
        app(
          StaticView(
            const AppStatus(Phase.failed, message: 'Сбой. Повторите.'),
          ),
          updates: FixedUpdates(update(UpdatePhase.available)),
        ),
      );
      expect(tester.getCenter(find.byKey(buttonKey)), before);
      expect(tester.getSize(find.byKey(buttonKey)), sizeBefore);
    });

    testWidgets('the button follows the size of the window', (tester) async {
      windowOf(tester, const Size(324, 521));
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
      final narrow = tester.getSize(find.byKey(buttonKey)).width;

      windowOf(tester, const Size(800, 300));
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
      final low = tester.getSize(find.byKey(buttonKey)).width;

      expect(narrow, greaterThan(150));
      expect(
        low,
        lessThan(130),
        reason: 'in a low window it leaves room above and below',
      );
    });
  });

  group('the button', () {
    testWidgets('connects when pressed off, disconnects when pressed on', (
      tester,
    ) async {
      windowOf(tester, window);
      final idle = StaticView(const AppStatus(Phase.idle));
      await tester.pumpWidget(app(idle));
      await tester.tap(find.byKey(buttonKey));
      expect(idle.presses, ['connect']);

      final up = StaticView(AppStatus(Phase.connected, region: 'sto'));
      await tester.pumpWidget(app(up));
      await tester.tap(find.byKey(buttonKey));
      expect(up.presses, ['disconnect']);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('calls off a connection or a move that is on its way', (
      tester,
    ) async {
      windowOf(tester, window);
      for (final phase in [Phase.connecting, Phase.switching]) {
        final v = StaticView(AppStatus(phase, region: 'sto'));
        await tester.pumpWidget(app(v));
        await tester.tap(find.byKey(buttonKey));
        expect(v.presses, ['cancel'], reason: '$phase');
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('after a failure tries again', (tester) async {
      windowOf(tester, window);
      final v = StaticView(
        const AppStatus(Phase.failed, message: 'Сервер не отвечает.'),
      );
      await tester.pumpWidget(app(v));
      await tester.tap(find.byKey(buttonKey));
      expect(v.presses, ['connect']);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('has nothing to add while disconnecting', (tester) async {
      windowOf(tester, window);
      final v = StaticView(const AppStatus(Phase.disconnecting));
      await tester.pumpWidget(app(v));
      await tester.tap(find.byKey(buttonKey), warnIfMissed: false);
      expect(v.presses, isEmpty);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('says what it does to a screen reader, in each state', (
      tester,
    ) async {
      windowOf(tester, window);
      final handle = tester.ensureSemantics();
      final labels = {
        Phase.idle: 'Подключить',
        Phase.failed: 'Подключить',
        Phase.connected: 'Отключить',
        Phase.connecting: 'Отменить',
        Phase.switching: 'Отменить',
        Phase.disconnecting: 'Отключаюсь',
      };
      for (final e in labels.entries) {
        await tester.pumpWidget(app(StaticView(AppStatus(e.key))));
        expect(
          find.bySemanticsLabel(e.value),
          findsWidgets,
          reason: '${e.key}',
        );
      }
      await tester.pumpWidget(const SizedBox());
      handle.dispose();
    });
  });

  group('the bottom of the window', () {
    testWidgets('shows what went wrong and what to do, and nothing else', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(
            const AppStatus(
              Phase.failed,
              message:
                  'Сервер не ответил на регистрацию. Проверьте интернет и повторите.',
            ),
          ),
        ),
      );
      expect(find.text('Сервер не ответил на регистрацию.'), findsOneWidget);
      expect(find.text('Проверьте интернет и повторите.'), findsOneWidget);
      expect(find.byType(Text), findsNWidgets(2));
    });

    testWidgets('is a bar along the bottom edge, as wide as the window', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(
            const AppStatus(Phase.failed, message: 'Сбой. Повторите.'),
          ),
        ),
      );
      final bar = tester.getRect(find.byKey(const ValueKey('error-panel')));
      expect(bar.bottom, window.height);
      expect(bar.left, 0);
      expect(bar.right, window.width);
    });

    testWidgets('says something even when the failure came without words', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.failed))));
      expect(find.textContaining('Не удалось подключиться'), findsOneWidget);
    });

    testWidgets(
      'a machine that cannot run the tunnel says why before anything is pressed',
      (tester) async {
        windowOf(tester, window);
        await tester.pumpWidget(
          app(
            StaticView(
              const AppStatus(
                Phase.idle,
                note:
                    'Нужны права администратора. Закройте Rebirth и запустите его заново.',
              ),
            ),
          ),
        );
        expect(find.textContaining('администратора'), findsOneWidget);
      },
    );

    testWidgets('a move that did not work out is said, but the tunnel stays on', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(
            AppStatus(
              Phase.connected,
              region: 'sto',
              note:
                  'Новый сервер пока не отвечает, остаёмся на прежнем. Попробуем снова через минуту.',
            ),
          ),
        ),
      );
      expect(find.textContaining('остаёмся на прежнем'), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('a failed update is said here too, under the connection', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(const AppStatus(Phase.idle)),
          updates: FixedUpdates(
            update(
              UpdatePhase.failed,
              message: 'Нет связи с сервером обновления. Проверьте интернет.',
            ),
          ),
        ),
      );
      expect(
        find.text('Нет связи с сервером обновления.'),
        findsOneWidget,
        reason: 'the error is at the bottom, where all of them are',
      );
    });

    testWidgets(
      'a problem with the connection comes before one with the update',
      (tester) async {
        windowOf(tester, window);
        await tester.pumpWidget(
          app(
            StaticView(
              const AppStatus(
                Phase.failed,
                message: 'Нет связи. Проверьте сеть.',
              ),
            ),
            updates: FixedUpdates(
              update(UpdatePhase.failed, message: 'Сбой загрузки. Повторите.'),
            ),
          ),
        );
        expect(find.text('Нет связи.'), findsOneWidget);
        expect(find.text('Сбой загрузки.'), findsNothing);
      },
    );
  });

  group('the top of the window', () {
    testWidgets('shows nothing without an update', (tester) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(const AppStatus(Phase.idle)),
          updates: FixedUpdates(null),
        ),
      );
      expect(find.byKey(const ValueKey('update-banner')), findsNothing);
      expect(find.byType(Text), findsNothing);
    });

    testWidgets('is a line along the top edge, as wide as the window', (
      tester,
    ) async {
      windowOf(tester, window);
      await tester.pumpWidget(
        app(
          StaticView(const AppStatus(Phase.idle)),
          updates: FixedUpdates(update(UpdatePhase.available)),
        ),
      );
      final line = tester.getRect(find.byKey(const ValueKey('update-banner')));
      expect(line.top, 0);
      expect(line.left, 0);
      expect(line.right, window.width);
      expect(line.height, lessThan(50), reason: 'a line, not a card');
    });
  });

  testWidgets('follows the controller from off to on and back', (tester) async {
    windowOf(tester, window);
    final v = LiveView();
    await tester.pumpWidget(app(v));

    v.set(const AppStatus(Phase.connecting, message: 'Получаю настройки…'));
    await tester.pump();
    await tester.tap(find.byKey(buttonKey));
    expect(v.presses, ['cancel']);

    v.set(AppStatus(Phase.connected, region: 'ams', since: DateTime.now()));
    await tester.pump();
    await tester.tap(find.byKey(buttonKey));
    expect(v.presses.last, 'disconnect');

    v.set(const AppStatus(Phase.idle));
    await tester.pump();
    await tester.tap(find.byKey(buttonKey));
    expect(v.presses.last, 'connect');
    expect(find.byType(Text), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });
}
