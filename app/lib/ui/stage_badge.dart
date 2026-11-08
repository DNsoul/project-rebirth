import 'dart:math' as math;
import 'dart:ui' show lerpDouble;

import 'package:flutter/material.dart';

import '../core/controller.dart' show ConnectStep;
import 'theme.dart';

/// The sign of each stage of making a connection.
IconData stepIcon(ConnectStep step) => switch (step) {
  ConnectStep.settings => Icons.cloud_download_rounded,
  ConnectStep.tunnel => Icons.electrical_services_rounded,
  ConnectStep.server => Icons.dns_rounded,
  ConnectStep.traffic => Icons.swap_horiz_rounded,
};

/// The disc that rides on the rim of the button while a connection is made, with
/// the sign of the stage being done in it. It rests at one of four places (top,
/// right, bottom, left) and slides to the next when a stage is done. Each sign
/// moves in its own way, so that a glance says which stage it is and that it is
/// alive: the cloud sinks, the plug goes in and out, the server sends rings
/// outwards, the arrows pass each other. After a failure it is red and has a
/// cross, at the place where it broke.
///
/// It fills a square of [diameter] over the button and is placed in it by
/// [progress] (in quarters of the rim, 0 at the top, going clockwise).
class StageBadge extends StatelessWidget {
  const StageBadge({
    super.key,
    required this.diameter,
    required this.progress,
    required this.amount,
    required this.color,
    required this.step,
    required this.broken,
    required this.clock,
    required this.time,
  });

  /// The width of the button the disc rides on.
  final double diameter;

  final double progress;

  /// How visible it is, from 0 to 1.
  final double amount;

  final Color color;
  final ConnectStep step;
  final bool broken;

  /// Seconds since the stage's clock started, and how to read it (a person who
  /// asked for no motion gets one still moment).
  final ValueNotifier<double> clock;
  final double Function(double) time;

  /// The width of the disc, in widths of the button.
  static const share = 0.22;

  /// How far in from the edge of the button the middle of the rim is: half the
  /// width of the line `RingPainter` draws.
  static const _rimInset = 1.5;

  @override
  Widget build(BuildContext context) {
    final size = diameter * share;
    final radius = diameter / 2 - _rimInset;
    final angle = -math.pi / 2 + progress.clamp(0.0, 4.0) * math.pi / 2;
    final centre = Offset(
      diameter / 2 + math.cos(angle) * radius,
      diameter / 2 + math.sin(angle) * radius,
    );
    return SizedBox(
      width: diameter,
      height: diameter,
      child: Stack(
        clipBehavior: Clip.none,
        children: [
          Positioned(
            left: centre.dx - size / 2,
            top: centre.dy - size / 2,
            width: size,
            height: size,
            child: Opacity(
              opacity: amount.clamp(0.0, 1.0),
              child: DecoratedBox(
                decoration: BoxDecoration(shape: BoxShape.circle, color: color),
                child: Center(
                  child: AnimatedSwitcher(
                    duration: const Duration(milliseconds: 300),
                    switchInCurve: Curves.easeOut,
                    transitionBuilder: (child, animation) => FadeTransition(
                      opacity: animation,
                      child: ScaleTransition(
                        scale: Tween<double>(
                          begin: 0.6,
                          end: 1,
                        ).animate(animation),
                        child: child,
                      ),
                    ),
                    child: _Glyph(
                      key: ValueKey<Object>(broken ? 'broken' : step),
                      step: step,
                      broken: broken,
                      size: size * 0.62,
                      clock: clock,
                      time: time,
                    ),
                  ),
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _Glyph extends StatelessWidget {
  const _Glyph({
    super.key,
    required this.step,
    required this.broken,
    required this.size,
    required this.clock,
    required this.time,
  });

  final ConnectStep step;
  final bool broken;
  final double size;
  final ValueNotifier<double> clock;
  final double Function(double) time;

  @override
  Widget build(BuildContext context) {
    if (broken) {
      return Icon(Icons.close_rounded, size: size, color: Ember.surface);
    }
    final icon = Icon(stepIcon(step), size: size, color: Ember.surface);
    return ValueListenableBuilder<double>(
      valueListenable: clock,
      builder: (context, raw, _) {
        final t = time(raw);
        switch (step) {
          case ConnectStep.settings:
            // Sinks and brightens, then starts again.
            final p = (t / 1.1) % 1.0;
            final q = Curves.easeInOut.transform(math.min(p / 0.6, 1.0));
            return Opacity(
              opacity: lerpDouble(0.5, 1, q)!,
              child: Transform.translate(
                offset: Offset(0, lerpDouble(-0.08, 0.08, q)! * size),
                child: icon,
              ),
            );
          case ConnectStep.tunnel:
            // Goes in and comes out.
            final w = 0.5 - 0.5 * math.cos(2 * math.pi * t / 1.0);
            return Transform.translate(
              offset: Offset(lerpDouble(-0.1, 0.08, w)! * size, 0),
              child: icon,
            );
          case ConnectStep.server:
            return SizedBox(
              width: size,
              height: size,
              child: Stack(
                clipBehavior: Clip.none,
                alignment: Alignment.center,
                children: [
                  Positioned.fill(
                    child: CustomPaint(
                      painter: _PingPainter(t: t, color: Ember.surface),
                    ),
                  ),
                  icon,
                ],
              ),
            );
          case ConnectStep.traffic:
            // There and back.
            final w = 0.5 - 0.5 * math.cos(2 * math.pi * t / 0.9);
            return Transform.translate(
              offset: Offset(lerpDouble(-0.1, 0.1, w)! * size, 0),
              child: icon,
            );
        }
      },
    );
  }
}

/// Two rings that grow from the sign and fade, one half a turn behind the other:
/// a call going out, waiting for an answer.
class _PingPainter extends CustomPainter {
  const _PingPainter({required this.t, required this.color});

  final double t;
  final Color color;

  static const _period = 1.6;

  @override
  void paint(Canvas canvas, Size size) {
    final c = size.center(Offset.zero);
    for (final lag in const [0.0, _period / 2]) {
      final p = ((t + lag) / _period) % 1.0;
      final scale = lerpDouble(0.6, 1.8, Curves.easeOut.transform(p))!;
      canvas.drawCircle(
        c,
        size.width / 2 * scale,
        Paint()
          ..style = PaintingStyle.stroke
          ..strokeWidth = 1.5
          ..color = color.withValues(alpha: lerpDouble(0.7, 0, p)!),
      );
    }
  }

  @override
  bool shouldRepaint(_PingPainter old) => old.t != t || old.color != color;
}
