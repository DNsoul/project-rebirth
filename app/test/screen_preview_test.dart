// Draws the screen in each state into PNG files, to look at. Not a check of
// anything: it only runs when BRAND_DIR names a folder, as brand_test.dart does.
//
//   $env:BRAND_DIR = "$PWD\build\preview"; flutter test test/screen_preview_test.dart
//
// The fire moves, so each picture is taken at a moment chosen to show it at its
// brightest (a few are taken at two moments).

import 'dart:async';
import 'dart:io';
import 'dart:ui' as ui;

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/updater.dart';
import 'package:awg_client/ui/home_page.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

class _View implements ConnectionView {
  _View(this.status);

  @override
  final AppStatus status;

  @override
  Stream<AppStatus> get statusStream => const Stream.empty();

  @override
  Future<void> connect() async {}

  @override
  Future<void> disconnect() async {}

  @override
  Future<void> cancel() async {}
}

class _Updates implements UpdateView {
  _Updates(this.status);

  @override
  final UpdateStatus? status;

  @override
  Stream<UpdateStatus?> get statusStream => const Stream.empty();

  @override
  Future<void> install() async {}
}

class _Shot {
  const _Shot(this.name, this.status, {this.update, this.at = 1700});

  final String name;
  final AppStatus status;
  final UpdateStatus? update;

  /// The moment, in milliseconds, at which to take it.
  final int at;
}

void main() {
  final dir = Platform.environment['BRAND_DIR'];

  testWidgets('draws the screen', (tester) async {
    if (dir == null || dir.isEmpty) {
      return;
    }
    final font = File(r'C:\Windows\Fonts\segoeui.ttf');
    if (font.existsSync()) {
      final loader = FontLoader('Roboto')
        ..addFont(Future.value(ByteData.sublistView(font.readAsBytesSync())));
      await loader.load();
    }
    final icons = File(
      '${Platform.environment['FLUTTER_ROOT']}/bin/cache/artifacts/material_fonts/materialicons-regular.otf',
    );
    if (icons.existsSync()) {
      final loader = FontLoader('MaterialIcons')
        ..addFont(Future.value(ByteData.sublistView(icons.readAsBytesSync())));
      await loader.load();
    }
    Directory(dir).createSync(recursive: true);

    final r = unsignedRelease(notes: 'Быстрее подключается.');
    UpdateOffer offer({bool mandatory = false}) => UpdateOffer(
      release: r,
      asset: r.asset(osWindows)!,
      mandatory: mandatory,
    );
    final since = DateTime.now();

    final shots = <_Shot>[
      const _Shot('1-idle', AppStatus(Phase.idle)),
      const _Shot(
        '2-connecting',
        AppStatus(
          Phase.connecting,
          message: 'Читаю настройки…',
          step: ConnectStep.settings,
        ),
        at: 350,
      ),
      const _Shot(
        '2b-connecting',
        AppStatus(
          Phase.connecting,
          message: 'Поднимаю туннель…',
          step: ConnectStep.tunnel,
        ),
        at: 350,
      ),
      const _Shot(
        '2c-connecting',
        AppStatus(
          Phase.connecting,
          message: 'Жду ответа сервера…',
          step: ConnectStep.server,
        ),
        at: 350,
      ),
      const _Shot(
        '2d-connecting',
        AppStatus(
          Phase.connecting,
          message: 'Проверяю, что трафик идёт…',
          step: ConnectStep.traffic,
        ),
        at: 350,
      ),
      _Shot(
        '3-connected',
        AppStatus(Phase.connected, region: 'sto', since: since),
      ),
      _Shot(
        '4-switching',
        AppStatus(Phase.switching, region: 'sto', since: since),
        at: 450,
      ),
      _Shot(
        '5-silent',
        AppStatus(
          Phase.connected,
          region: 'sto',
          since: since,
          lostContact: true,
        ),
        at: 600,
      ),
      _Shot(
        '5b-recovering',
        AppStatus(
          Phase.connected,
          region: 'sto',
          since: since,
          lostContact: true,
          recovering: true,
        ),
        at: 450,
      ),
      const _Shot('6-disconnecting', AppStatus(Phase.disconnecting), at: 600),
      const _Shot(
        '7-failed',
        AppStatus(
          Phase.failed,
          message:
              'Сервер не ответил на регистрацию. Проверьте интернет и повторите.',
          step: ConnectStep.server,
        ),
        at: 140,
      ),
      const _Shot(
        '7b-failed-first-step',
        AppStatus(
          Phase.failed,
          message: 'Нет связи с GitHub. Проверьте интернет и повторите.',
          step: ConnectStep.settings,
        ),
        at: 140,
      ),
      const _Shot(
        '7c-waiting-for-owner',
        AppStatus(
          Phase.connecting,
          message: 'Ждём одобрения владельца. Код для сверки: 3045',
          step: ConnectStep.server,
          approvalCode: '3045',
        ),
        at: 350,
      ),
      const _Shot(
        '7d-refused',
        AppStatus(
          Phase.failed,
          message: 'Владелец закрыл доступ к VPN для этого устройства.',
          step: ConnectStep.server,
        ),
        at: 140,
      ),
      _Shot(
        '8-connected-update',
        AppStatus(Phase.connected, region: 'sto', since: since),
        update: UpdateStatus(UpdatePhase.available, offer()),
      ),
      _Shot(
        '9-downloading',
        AppStatus(Phase.connected, region: 'sto', since: since),
        update: UpdateStatus(UpdatePhase.downloading, offer(), progress: 0.42),
      ),
      const _Shot(
        'a-idle-warning',
        AppStatus(
          Phase.idle,
          note:
              'Нужны права администратора. Закройте Rebirth и запустите его заново: Windows спросит разрешение.',
        ),
      ),
      _Shot(
        'b-mandatory-failed',
        const AppStatus(Phase.idle),
        update: UpdateStatus(
          UpdatePhase.failed,
          offer(mandatory: true),
          message:
              'Скачанный файл не прошёл проверку. Установка отменена, повторите позже.',
        ),
      ),
    ];

    Future<void> render(List<_Shot> list, Size size, String file) async {
      final key = GlobalKey();
      tester.view.physicalSize =
          Size(size.width * list.length, size.height) * 2;
      tester.view.devicePixelRatio = 2;
      await tester.pumpWidget(
        RepaintBoundary(
          key: key,
          child: Directionality(
            textDirection: TextDirection.ltr,
            child: Row(
              children: [
                for (final s in list)
                  SizedBox(
                    width: size.width,
                    height: size.height,
                    child: MaterialApp(
                      debugShowCheckedModeBanner: false,
                      theme: appTheme(),
                      home: HomePage(
                        key: UniqueKey(),
                        controller: _View(s.status),
                        updates: s.update == null ? null : _Updates(s.update),
                      ),
                    ),
                  ),
              ],
            ),
          ),
        ),
      );
      // One moment for the whole sheet; the pictures that want another take it
      // as the nearest of the two common ones.
      await tester.pump(Duration(milliseconds: list.first.at));
      await tester.runAsync(() async {
        final boundary =
            key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
        final image = await boundary.toImage(pixelRatio: 1.5);
        final png = (await image.toByteData(format: ui.ImageByteFormat.png))!;
        File('$dir/$file').writeAsBytesSync(png.buffer.asUint8List());
      });
      await tester.pumpWidget(const SizedBox());
    }

    addTearDown(tester.view.reset);

    // The window as it will be: 340 x 560 outside, a little less inside.
    const window = Size(324, 521);
    // One sheet per picture, so that each can be taken at its own moment.
    for (final s in shots) {
      await render([s], window, 'screen-${s.name}.png');
    }
    // And the same on a phone, for the shape of it.
    _Shot named(String name) => shots.firstWhere((s) => s.name == name);
    await render(
      [named('3-connected')],
      const Size(360, 780),
      'phone-connected.png',
    );
    await render(
      [named('8-connected-update')],
      const Size(360, 780),
      'phone-update.png',
    );
    await render(
      [named('7-failed')],
      const Size(360, 640),
      'phone-failed-short.png',
    );
    await render(
      [named('2c-connecting')],
      const Size(360, 780),
      'phone-connecting.png',
    );
    await render(
      [named('7c-waiting-for-owner')],
      const Size(360, 780),
      'phone-waiting-for-owner.png',
    );
    // Recovering beside connected and lost contact, to compare.
    await render(
      [named('3-connected'), named('5b-recovering'), named('5-silent')],
      const Size(360, 780),
      'phone-recovering-compare.png',
    );
    await render(
      [named('7d-refused')],
      const Size(360, 780),
      'phone-refused.png',
    );
  });
}
