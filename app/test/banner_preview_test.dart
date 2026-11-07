// Draws the home screen with the update banner into PNG files to look at.
// Not a check of anything: it only runs when BRAND_DIR names a folder, as
// brand_test.dart does, and otherwise stays out of the way.
//
//   $env:BRAND_DIR = "$PWD\build\preview"; flutter test test/banner_preview_test.dart

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

  @override
  void dismiss() {}
}

void main() {
  final dir = Platform.environment['BRAND_DIR'];

  testWidgets('draws the banner', (tester) async {
    if (dir == null || dir.isEmpty) {
      return;
    }
    // Real letters instead of the test font's boxes.
    final font = File(r'C:\Windows\Fonts\segoeui.ttf');
    if (font.existsSync()) {
      final loader = FontLoader('Roboto')
        ..addFont(Future.value(ByteData.sublistView(font.readAsBytesSync())));
      await loader.load();
    }
    Directory(dir).createSync(recursive: true);

    final r = unsignedRelease(
      notes: 'Быстрее подключается, исправлена смена сервера.',
    );
    UpdateOffer offer({bool mandatory = false}) => UpdateOffer(
      release: r,
      asset: r.asset(osWindows)!,
      mandatory: mandatory,
    );

    final shots = <String, (AppStatus, UpdateStatus?)>{
      'offer': (
        const AppStatus(Phase.idle),
        UpdateStatus(UpdatePhase.available, offer()),
      ),
      'mandatory': (
        const AppStatus(Phase.idle),
        UpdateStatus(UpdatePhase.available, offer(mandatory: true)),
      ),
      'downloading': (
        AppStatus(Phase.connected, region: 'sto', since: DateTime.now()),
        UpdateStatus(UpdatePhase.downloading, offer(), progress: 0.42),
      ),
      'failed': (
        const AppStatus(Phase.idle),
        UpdateStatus(
          UpdatePhase.failed,
          offer(),
          message:
              'Скачанный файл не прошёл проверку: контрольная сумма не совпала.',
        ),
      ),
      'none': (const AppStatus(Phase.idle), null),
    };
    final sizes = {
      'phone': const Size(360, 780),
      'window': const Size(420, 640),
    };

    for (final size in sizes.entries) {
      for (final shot in shots.entries) {
        tester.view.physicalSize = size.value * 2;
        tester.view.devicePixelRatio = 2;
        final key = GlobalKey();
        await tester.pumpWidget(
          RepaintBoundary(
            key: key,
            child: MaterialApp(
              debugShowCheckedModeBanner: false,
              theme: appTheme(),
              home: HomePage(
                controller: _View(shot.value.$1),
                hint: 'Выйти можно из меню значка.',
                updates: _Updates(shot.value.$2),
              ),
            ),
          ),
        );
        await tester.pump(const Duration(milliseconds: 500));
        await tester.runAsync(() async {
          final boundary =
              key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
          final image = await boundary.toImage(pixelRatio: 1);
          final bytes = (await image.toByteData(
            format: ui.ImageByteFormat.png,
          ))!;
          File(
            '$dir/banner-${size.key}-${shot.key}.png',
          ).writeAsBytesSync(bytes.buffer.asUint8List());
        });
        expect(
          tester.takeException(),
          isNull,
          reason: '${size.key} ${shot.key}',
        );
      }
    }
    addTearDown(tester.view.reset);
  });
}
