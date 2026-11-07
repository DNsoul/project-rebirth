import 'dart:io';
import 'dart:ui' as ui;

import 'package:awg_client/ui/rebirth_mark.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

/// Not a check: with BRAND_DIR set, draws every picture the app's icons are made
/// from, as PNG files. scripts/make-icons.ps1 runs it and turns the pictures into
/// the .ico files and the Android resources.
///
/// The drawing is the one the screen uses (lib/ui/rebirth_mark.dart), so the icon
/// and the app cannot drift apart.
void main() {
  final out = Platform.environment['BRAND_DIR'];

  Future<void> save(
    String name,
    int size,
    void Function(Canvas c, double s) draw,
  ) async {
    final recorder = ui.PictureRecorder();
    final canvas = Canvas(recorder);
    draw(canvas, size.toDouble());
    final image = await recorder.endRecording().toImage(size, size);
    final bytes = await image.toByteData(format: ui.ImageByteFormat.png);
    File('$out/$name.png').writeAsBytesSync(bytes!.buffer.asUint8List());
  }

  /// The mark, [fraction] of the way across a square of side [s], in the middle.
  void mark(
    Canvas c,
    double s,
    double fraction,
    RebirthColors colors, {
    double glow = 0,
  }) {
    final side = s * fraction;
    c.save();
    c.translate((s - side) / 2, (s - side) / 2);
    RebirthPainter(colors: colors, glow: glow).paint(c, Size(side, side));
    c.restore();
  }

  /// A rounded square of soot with a warm light behind the middle.
  void tile(Canvas c, double s, {double radius = 0.22}) {
    final rect = Rect.fromLTWH(0, 0, s, s);
    final shape = RRect.fromRectAndRadius(rect, Radius.circular(s * radius));
    c.drawRRect(shape, Paint()..color = Ember.background);
    c.save();
    c.clipRRect(shape);
    c.drawRect(
      rect,
      Paint()
        ..shader = RadialGradient(
          center: const Alignment(0, -0.1),
          radius: 0.75,
          colors: [
            Ember.orange.withValues(alpha: 0.30),
            Ember.orange.withValues(alpha: 0),
          ],
        ).createShader(rect),
    );
    c.restore();
    c.drawRRect(
      shape.deflate(s * 0.012),
      Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = s * 0.018
        ..color = Ember.outline,
    );
  }

  const darkMark = Color(0xFF1A0F0B);

  testWidgets('draws the pictures for the icons', (tester) async {
    await tester.runAsync(() async {
      Directory(out!).createSync(recursive: true);

      // The program's own icon: the bird on its tile.
      for (final s in [16, 24, 32, 48, 64, 128, 256, 512, 1024]) {
        await save('app-$s', s, (c, d) {
          tile(c, d);
          mark(c, d, 0.80, RebirthColors.flame, glow: s >= 48 ? 0.7 : 0);
        });
      }

      // The tray: states are told apart by colour, so each is a disc of its own.
      // A lit disc with the dark bird on it reads at 16 pixels where a ring and a
      // thin glyph would not.
      const trayDiscs = {
        'idle': Color(0xFF6B5E57),
        'connected': Color(0xFFFF9A3C),
        'error': Color(0xFFD93030),
      };
      for (final s in [16, 20, 24, 32, 40, 48]) {
        for (final e in trayDiscs.entries) {
          await save('tray-${e.key}-$s', s, (c, d) {
            c.drawCircle(
              Offset(d / 2, d / 2),
              d / 2 - 0.5,
              Paint()..color = e.value,
            );
            mark(c, d, 0.70, RebirthColors.solid(darkMark));
          });
        }
        // Busy is a dark disc with an amber ring and an amber bird: the one
        // state that is not a solid colour, which is what makes it stand out.
        await save('tray-busy-$s', s, (c, d) {
          c.drawCircle(
            Offset(d / 2, d / 2),
            d / 2 - 0.5,
            Paint()..color = Ember.surface,
          );
          c.drawCircle(
            Offset(d / 2, d / 2),
            d / 2 - d * 0.07,
            Paint()
              ..style = PaintingStyle.stroke
              ..strokeWidth = d * 0.11
              ..color = Ember.amber,
          );
          mark(c, d, 0.58, RebirthColors.solid(Ember.amber));
        });
      }

      // Android. The launcher icon of older phones, and the two layers of the
      // adaptive icon of newer ones: the bird goes in the middle two thirds, the
      // part the system never crops.
      for (final s in [48, 72, 96, 144, 192]) {
        await save('android-legacy-$s', s, (c, d) {
          tile(c, d, radius: 0.18);
          mark(c, d, 0.76, RebirthColors.flame, glow: 0.5);
        });
      }
      await save('android-foreground-432', 432, (c, d) {
        mark(c, d, 0.58, RebirthColors.flame, glow: 0.8);
      });

      // The small white shape in the status bar and the notification, which the
      // system colours itself: the outline of the bird and nothing else.
      for (final s in [24, 36, 48, 72, 96]) {
        await save('android-stat-$s', s, (c, d) {
          final path = rebirthPath();
          final scale = d * 0.92 / 100;
          c.translate(d * 0.04, d * 0.04);
          c.scale(scale);
          c.drawPath(path, Paint()..color = Colors.white);
        });
      }
    });
  }, skip: out == null);
}
