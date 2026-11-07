import 'dart:async';
import 'dart:io';
import 'dart:ui' as ui;

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/ui/home_page.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

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

Widget app(ConnectionView view, {Brightness brightness = Brightness.light}) =>
    MaterialApp(
      debugShowCheckedModeBanner: false,
      theme: appTheme(brightness),
      // A fresh screen each time: a stream builder keeps what it last showed
      // when the widget above it is swapped for another of the same kind.
      home: HomePage(key: UniqueKey(), controller: view),
    );

final since = DateTime.now().subtract(const Duration(minutes: 12, seconds: 7));

void main() {
  group('the screen', () {
    testWidgets('starts as "disconnected" with one button', (tester) async {
      await tester.pumpWidget(app(StaticView(const AppStatus(Phase.idle))));
      expect(find.text('Отключено'), findsOneWidget);
      expect(find.byKey(const ValueKey('power-button')), findsOneWidget);
      expect(find.text('Повторить'), findsNothing);
    });

    testWidgets(
      'pressing the button connects, and when connected disconnects',
      (tester) async {
        final idle = StaticView(const AppStatus(Phase.idle));
        await tester.pumpWidget(app(idle));
        await tester.tap(find.byKey(const ValueKey('power-button')));
        expect(idle.presses, ['connect']);

        final up = StaticView(
          AppStatus(Phase.connected, region: 'sto', since: since),
        );
        await tester.pumpWidget(app(up));
        await tester.tap(find.byKey(const ValueKey('power-button')));
        expect(up.presses, ['disconnect']);
      },
    );

    testWidgets(
      'while connecting or moving, the button calls it off, and says so',
      (tester) async {
        for (final phase in [Phase.connecting, Phase.switching]) {
          final v = StaticView(
            AppStatus(phase, message: 'Поднимаю туннель…', region: 'sto'),
          );
          await tester.pumpWidget(app(v));
          expect(
            find.text('Нажмите кнопку, чтобы отменить.'),
            findsOneWidget,
            reason: '$phase',
          );
          await tester.tap(find.byKey(const ValueKey('power-button')));
          expect(v.presses, ['cancel'], reason: '$phase');
        }
      },
    );

    testWidgets('while disconnecting, a second press has nothing to add', (
      tester,
    ) async {
      final v = StaticView(
        const AppStatus(Phase.disconnecting, message: 'Отключаю туннель…'),
      );
      await tester.pumpWidget(app(v));
      await tester.tap(
        find.byKey(const ValueKey('power-button')),
        warnIfMissed: false,
      );
      expect(v.presses, isEmpty);
      expect(find.text('Нажмите кнопку, чтобы отменить.'), findsNothing);
    });

    testWidgets('says which step it is on', (tester) async {
      final v = LiveView();
      await tester.pumpWidget(app(v));
      for (final step in [
        'Читаю настройки…',
        'Регистрирую это устройство…',
        'Поднимаю туннель…',
        'Жду ответа сервера…',
        'Проверяю, что трафик идёт…',
        'Включаю защиту имён…',
      ]) {
        v.set(AppStatus(Phase.connecting, message: step));
        await tester.pump();
        expect(find.text(step), findsOneWidget);
        expect(find.text('Подключаюсь…'), findsOneWidget);
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('shows the server and how long it has been up', (tester) async {
      await tester.pumpWidget(
        app(
          StaticView(AppStatus(Phase.connected, region: 'waw', since: since)),
        ),
      );
      expect(find.text('Подключено'), findsOneWidget);
      expect(find.text('Сервер: Варшава'), findsOneWidget);
      expect(find.textContaining('В сети 12:'), findsOneWidget);
    });

    testWidgets('a failure says what went wrong and offers to try again', (
      tester,
    ) async {
      final v = StaticView(
        const AppStatus(Phase.failed, message: 'Сервер не отвечает.'),
      );
      await tester.pumpWidget(app(v));
      expect(find.text('Не получилось'), findsOneWidget);
      expect(find.text('Сервер не отвечает.'), findsOneWidget);
      await tester.tap(find.text('Повторить'));
      expect(v.presses, ['connect']);
    });

    testWidgets(
      'a silent server is said to be silent, and a move that failed is explained',
      (tester) async {
        await tester.pumpWidget(
          app(
            StaticView(
              AppStatus(
                Phase.connected,
                region: 'sto',
                since: since,
                lostContact: true,
              ),
            ),
          ),
        );
        expect(find.text('Нет ответа от сервера'), findsOneWidget);
        expect(find.textContaining('Ищу замену'), findsOneWidget);

        await tester.pumpWidget(
          app(
            StaticView(
              AppStatus(
                Phase.connected,
                region: 'sto',
                since: since,
                note: 'Новый сервер пока не отвечает, остаюсь на прежнем.',
              ),
            ),
          ),
        );
        expect(find.textContaining('остаюсь на прежнем'), findsOneWidget);
      },
    );

    testWidgets(
      'a machine that cannot run the tunnel says why before anything is pressed',
      (tester) async {
        await tester.pumpWidget(
          app(
            StaticView(
              const AppStatus(
                Phase.idle,
                note: 'Нужно запускать от имени администратора.',
              ),
            ),
          ),
        );
        expect(find.textContaining('администратора'), findsOneWidget);
        expect(find.text('Отключено'), findsOneWidget);
      },
    );

    testWidgets('follows the controller from idle to connected and back', (
      tester,
    ) async {
      final v = LiveView();
      await tester.pumpWidget(app(v));
      expect(find.text('Отключено'), findsOneWidget);

      v.set(const AppStatus(Phase.connecting, message: 'Получаю настройки…'));
      await tester.pump();
      expect(find.text('Подключаюсь…'), findsOneWidget);
      expect(find.text('Получаю настройки…'), findsOneWidget);

      v.set(AppStatus(Phase.connected, region: 'ams', since: DateTime.now()));
      await tester.pump();
      expect(find.text('Подключено'), findsOneWidget);
      expect(find.text('Сервер: Амстердам'), findsOneWidget);

      v.set(
        AppStatus(
          Phase.switching,
          region: 'ams',
          message: 'Перехожу на новый сервер…',
          since: DateTime.now(),
        ),
      );
      await tester.pump();
      expect(find.text('Меняю сервер…'), findsOneWidget);

      v.set(const AppStatus(Phase.idle));
      await tester.pump();
      expect(find.text('Отключено'), findsOneWidget);
      // Take the screen down so the uptime timer stops before the test ends.
      await tester.pumpWidget(const SizedBox());
    });
  });

  // Not a check: with SCREENSHOT_DIR set, draws every state to a picture so the
  // layout can be looked at. A test run has no real fonts, so it borrows the
  // ones that ship with Flutter.
  testWidgets(
    'draws the screen in each state',
    (tester) async {
      final dir = Platform.environment['SCREENSHOT_DIR']!;
      final root = Platform.environment['FLUTTER_ROOT']!;
      Future<void> load(String family, String file) async {
        final bytes = File(
          '$root/bin/cache/artifacts/material_fonts/$file',
        ).readAsBytesSync();
        final loader = FontLoader(family)
          ..addFont(Future.value(ByteData.sublistView(bytes)));
        await loader.load();
      }

      await load('Roboto', 'roboto-regular.ttf');
      await load('MaterialIcons', 'materialicons-regular.otf');

      final states = <AppStatus>[
        const AppStatus(Phase.idle),
        const AppStatus(
          Phase.connecting,
          message: 'Регистрирую это устройство на сервере…',
        ),
        AppStatus(Phase.connected, region: 'waw', since: since),
        AppStatus(
          Phase.connected,
          region: 'sto',
          since: since,
          lostContact: true,
        ),
        const AppStatus(
          Phase.failed,
          message:
              'Сервер не отвечает на регистрацию. Проверь связь и попробуй ещё раз.',
        ),
      ];

      addTearDown(tester.view.reset);
      for (final brightness in Brightness.values) {
        final key = GlobalKey();
        tester.view.physicalSize = Size(420.0 * states.length, 620);
        tester.view.devicePixelRatio = 1;
        await tester.pumpWidget(
          RepaintBoundary(
            key: key,
            child: Directionality(
              textDirection: TextDirection.ltr,
              child: Row(
                children: [
                  for (final s in states)
                    SizedBox(
                      width: 420,
                      height: 620,
                      child: app(StaticView(s), brightness: brightness),
                    ),
                ],
              ),
            ),
          ),
        );
        await tester.pump(const Duration(milliseconds: 500));
        await tester.runAsync(() async {
          final boundary =
              key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
          final image = await boundary.toImage();
          final png = await image.toByteData(format: ui.ImageByteFormat.png);
          await File(
            '$dir/screen-${brightness.name}.png',
          ).writeAsBytes(png!.buffer.asUint8List());
        });
        await tester.pumpWidget(const SizedBox());
      }
    },
    skip: Platform.environment['SCREENSHOT_DIR'] == null,
  );
}
