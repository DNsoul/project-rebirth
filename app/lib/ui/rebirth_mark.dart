import 'dart:math' as math;
import 'dart:typed_data';

import 'package:flutter/material.dart';

/// The mark of the app: a phoenix with its wings raised, drawn from curves so
/// that it is sharp at any size, from the 16 pixels of a tray icon to the splash
/// of a store listing. The same drawing is on the screen, in the icons and in the
/// installer.
///
/// It is drawn in a square of 100 units, symmetric about the line x = 50.

/// The outline of the whole bird: the body with its head and crest, the two
/// wings of four feathers each, and the tail of three flames.
Path rebirthPath() {
  final right = Path();

  // Wing: four feathers fanning out from the shoulder, each a curved blade that
  // bends upward, the top one longest. They start at separate points, so that the
  // wing opens like a fan instead of running together.
  const feathers = <(Offset, Offset, double, double)>[
    (Offset(56, 37), Offset(89, 3), 0.15, 0.17),
    (Offset(57, 42), Offset(99, 19), 0.13, 0.17),
    (Offset(57, 47), Offset(99, 37), 0.12, 0.18),
    (Offset(56, 52), Offset(91, 56), 0.10, 0.2),
  ];
  for (final (base, tip, bend, width) in feathers) {
    right.addPath(_feather(base, tip, bend: bend, width: width), Offset.zero);
  }

  // Tail: a flame to each side of the middle one, starting inside the body so
  // that they grow out of it.
  right.addPath(
    _flame(const Offset(51, 58), const Offset(68, 91), lean: 0.2),
    Offset.zero,
  );

  final left = right.transform(_mirror());

  final body = Path()
    ..moveTo(50, 3) // the crest
    ..cubicTo(55, 8, 58, 15, 56.5, 22) // the head
    ..cubicTo(61, 29, 62.5, 41, 58, 55) // the breast
    ..cubicTo(56, 63, 53, 70, 50, 74)
    ..cubicTo(47, 70, 44, 63, 42, 55)
    ..cubicTo(37.5, 41, 39, 29, 43.5, 22)
    ..cubicTo(42, 15, 45, 8, 50, 3)
    ..close();

  // The middle flame of the tail.
  final middle = Path()
    ..moveTo(46.5, 62)
    ..cubicTo(43.5, 78, 47, 91, 50, 99)
    ..cubicTo(53, 91, 56.5, 78, 53.5, 62)
    ..close();

  return Path.combine(
    PathOperation.union,
    Path.combine(
      PathOperation.union,
      Path.combine(PathOperation.union, right, left),
      body,
    ),
    middle,
  );
}

/// A blade from [base] to [tip], bent to one side by [bend] of its length and as
/// wide as [width] of it.
Path _feather(
  Offset base,
  Offset tip, {
  required double bend,
  required double width,
}) {
  final d = tip - base;
  final len = d.distance;
  final unit = d / len;
  // Perpendicular, pointing up and away: the side the feather bends towards.
  final n = Offset(unit.dy, -unit.dx);
  Offset along(double t, double side) => base + d * t + n * (len * side);
  return Path()
    ..moveTo(base.dx, base.dy)
    ..cubicTo(
      along(0.30, bend + width * 0.9).dx,
      along(0.30, bend + width * 0.9).dy,
      along(0.72, bend * 0.9 + width * 0.55).dx,
      along(0.72, bend * 0.9 + width * 0.55).dy,
      tip.dx,
      tip.dy,
    )
    ..cubicTo(
      along(0.78, bend * 0.9 - width * 0.35).dx,
      along(0.78, bend * 0.9 - width * 0.35).dy,
      along(0.34, bend - width * 0.9).dx,
      along(0.34, bend - width * 0.9).dy,
      base.dx,
      base.dy,
    )
    ..close();
}

/// A flame from [base] to [tip], leaning by [lean] of its length.
Path _flame(Offset base, Offset tip, {required double lean}) {
  final d = tip - base;
  final len = d.distance;
  final n = Offset(-d.dy, d.dx) / len;
  Offset at(double t, double side) => base + d * t + n * (len * side);
  return Path()
    ..moveTo(base.dx, base.dy)
    ..cubicTo(
      at(0.35, -lean * 0.2).dx,
      at(0.35, -lean * 0.2).dy,
      at(0.7, lean).dx,
      at(0.7, lean).dy,
      tip.dx,
      tip.dy,
    )
    ..cubicTo(
      at(0.55, lean * 0.1).dx,
      at(0.55, lean * 0.1).dy,
      at(0.45, -lean * 0.5).dx,
      at(0.45, -lean * 0.5).dy,
      base.dx,
      base.dy,
    )
    ..close();
}

/// The reflection about x = 50.
Float64List _mirror() => Float64List.fromList(<double>[
  -1, 0, 0, 0, //
  0, 1, 0, 0, //
  0, 0, 1, 0, //
  100, 0, 0, 1, //
]);

/// Colours of the mark.
class RebirthColors {
  const RebirthColors(this.top, this.middle, this.bottom);

  final Color top;
  final Color middle;
  final Color bottom;

  /// Gold at the crest, through orange, to red in the tail.
  static const flame = RebirthColors(
    Color(0xFFFFD36B),
    Color(0xFFFF8A3D),
    Color(0xFFE5322B),
  );

  /// A single colour, for places that want a plain shape.
  static RebirthColors solid(Color c) => RebirthColors(c, c, c);
}

class RebirthPainter extends CustomPainter {
  const RebirthPainter({this.colors = RebirthColors.flame, this.glow = 0});

  final RebirthColors colors;

  /// How far the mark glows, from 0 (not at all) to 1 (strongly). The glow is
  /// the mark's own shape, blurred, behind it.
  final double glow;

  @override
  void paint(Canvas canvas, Size size) {
    final side = math.min(size.width, size.height);
    canvas.save();
    canvas.translate((size.width - side) / 2, (size.height - side) / 2);
    canvas.scale(side / 100);

    final path = rebirthPath();
    if (glow > 0) {
      canvas.drawPath(
        path,
        Paint()
          ..color = colors.middle.withValues(alpha: 0.55 * glow)
          ..maskFilter = MaskFilter.blur(BlurStyle.normal, 6 + 6 * glow),
      );
    }
    canvas.drawPath(
      path,
      Paint()
        ..shader = LinearGradient(
          begin: Alignment.topCenter,
          end: Alignment.bottomCenter,
          colors: [colors.top, colors.middle, colors.bottom],
          stops: const [0.0, 0.5, 1.0],
        ).createShader(const Rect.fromLTWH(0, 0, 100, 100)),
    );
    // Lighter towards the tips of the wings and the crest, deeper in the middle:
    // the feathers glow at their ends.
    canvas.drawPath(
      path,
      Paint()
        ..shader = RadialGradient(
          center: const Alignment(0, -0.15),
          radius: 0.78,
          colors: [
            colors.top.withValues(alpha: 0),
            colors.top.withValues(alpha: 0.55),
          ],
          stops: const [0.45, 1.0],
        ).createShader(const Rect.fromLTWH(0, 0, 100, 100)),
    );
    // The eye: a gap in the head, so that it reads as a bird and not a flame.
    canvas.drawCircle(
      const Offset(52.4, 15.5),
      1.5,
      Paint()..color = const Color(0xFF140E0C),
    );
    canvas.restore();
  }

  @override
  bool shouldRepaint(RebirthPainter old) =>
      old.colors != colors || old.glow != glow;
}

/// The mark as a widget.
class RebirthMark extends StatelessWidget {
  const RebirthMark({
    super.key,
    this.size = 96,
    this.colors = RebirthColors.flame,
    this.glow = 0,
  });

  final double size;
  final RebirthColors colors;
  final double glow;

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      width: size,
      height: size,
      child: ExcludeSemantics(
        child: CustomPaint(
          painter: RebirthPainter(colors: colors, glow: glow),
        ),
      ),
    );
  }
}
