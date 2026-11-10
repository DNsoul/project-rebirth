import 'dart:math' as math;
import 'dart:ui' show lerpDouble;

import 'package:flutter/material.dart';
import 'package:flutter/scheduler.dart';

import '../core/controller.dart' show ConnectStep, Phase;
import 'rebirth_mark.dart';
import 'stage_badge.dart';
import 'theme.dart';

// The whole window is one button and the light of a fire round it. Nothing is
// written on it: what is going on is in how the button looks and how the light
// moves. Everything is flat: solid colours, no gradients, no blur, no shadows.
// The light is a few translucent circles, one outside the other.
//
//   off            dark and still. No light at all.
//   connecting     the fire is being lit: the light flares and flickers fast,
//                  sparks lift off, and a spark on the rim has become a small
//                  disc with the sign of the stage being done (settings, tunnel,
//                  server, traffic). The disc rests at one of four places on the
//                  rim and slides to the next when a stage is done; what has been
//                  done stays lit behind it.
//   connected      the fire burns: slow breathing light, a few sparks rising.
//   moving         the fire flares: a spark on the rim, quicker light, more sparks.
//   server silent  the fire sputters: weak and uneven, the rim is in dashes.
//   going off      the fire dies down: the light fades, a spark runs backwards.
//   failed         a red ember with a heartbeat, and an arrow round the rim that
//                  says "press to try again". The disc stays where it broke, red,
//                  with a cross.

/// The light of the fire.
@immutable
class FireLook {
  const FireLook({
    required this.color,
    required this.level,
    this.pulse = 0,
    this.period = 3,
    this.flicker = 0,
    this.embers = 0,
    this.heartbeat = 0,
  });

  final Color color;

  /// How strong it burns, from 0 (out) to 1.
  final double level;

  /// How much of that it breathes in and out, from 0 (steady) to 1 (all of it).
  final double pulse;

  /// Seconds for one breath.
  final double period;

  /// How unevenly it burns, from 0 (not at all) to 1.
  final double flicker;

  /// How many sparks are in the air, from 0 to 1.
  final double embers;

  /// 0 breathes smoothly; 1 beats twice and rests, like a heart.
  final double heartbeat;

  @override
  bool operator ==(Object other) =>
      other is FireLook &&
      other.color == color &&
      other.level == level &&
      other.pulse == pulse &&
      other.period == period &&
      other.flicker == flicker &&
      other.embers == embers &&
      other.heartbeat == heartbeat;

  @override
  int get hashCode =>
      Object.hash(color, level, pulse, period, flicker, embers, heartbeat);

  static FireLook lerp(FireLook a, FireLook b, double t) => FireLook(
    color: Color.lerp(a.color, b.color, t)!,
    level: lerpDouble(a.level, b.level, t)!,
    pulse: lerpDouble(a.pulse, b.pulse, t)!,
    period: lerpDouble(a.period, b.period, t)!,
    flicker: lerpDouble(a.flicker, b.flicker, t)!,
    embers: lerpDouble(a.embers, b.embers, t)!,
    heartbeat: lerpDouble(a.heartbeat, b.heartbeat, t)!,
  );

  /// Nothing burns.
  static const out = FireLook(color: Ember.orange, level: 0);
}

/// How the fire breathes at second [t], from 0 to 1, without the unevenness.
double fireWave(FireLook f, double t) {
  final p = (t / f.period) % 1.0;
  final breath = 0.5 - 0.5 * math.cos(2 * math.pi * p);
  return breath + (_heartbeat(p) - breath) * f.heartbeat;
}

/// How bright the fire is at second [t]: 0 when it is out, about 1 at its best.
double fireIntensity(FireLook f, double t) {
  if (f.level <= 0) {
    return 0;
  }
  final wave = fireWave(f, t);
  // Three waves that never line up, so that the unevenness does not repeat.
  final noise =
      (math.sin(t * 7.3) +
          0.7 * math.sin(t * 11.9 + 1.3) +
          0.4 * math.sin(t * 17.7 + 2.1)) /
      2.1;
  final v =
      f.level * (1 - f.pulse + f.pulse * wave) * (1 + 0.28 * f.flicker * noise);
  return v.clamp(0.0, 1.3);
}

/// Two quick beats and a rest, over one cycle from 0 to 1.
double _heartbeat(double p) {
  double bump(double centre, double width) =>
      math.exp(-math.pow((p - centre) / width, 2));
  return math.min(1.0, bump(0.07, 0.04) + 0.75 * bump(0.27, 0.045));
}

/// How the button and the light look in one state.
@immutable
class StageLook {
  const StageLook({
    required this.bird,
    required this.ring,
    required this.ringGap,
    required this.spinner,
    required this.spinnerAmount,
    required this.spinnerTurns,
    required this.retry,
    required this.fire,
    this.progress = 0,
    this.lit = 0,
    this.badge = 0,
    this.badgeColor = _amber,
  });

  /// The colour of the bird on the button.
  final Color bird;

  /// The ring round the button, and how much of it is missing: 0 is whole, 0.5
  /// is dashes.
  final Color ring;
  final double ringGap;

  /// A spark that runs round the ring: what colour, how visible, and how many
  /// turns a second (negative runs backwards).
  final Color spinner;
  final double spinnerAmount;
  final double spinnerTurns;

  /// The arrow round the rim that says "try again", and how visible it is.
  final double retry;

  final FireLook fire;

  /// How far round the rim the stages have got, in quarters of it: 0 is the top,
  /// 1 a quarter turn on, 4 the whole way. The disc with the sign of the stage
  /// sits at the end of what is lit.
  final double progress;

  /// How visible the lit part of the rim is, and the disc: from 0 to 1.
  final double lit;
  final double badge;

  /// The colour of the lit part and of the disc.
  final Color badgeColor;

  /// Whether anything moves, so that the clock has to run.
  bool get animated =>
      fire.level > 0.002 || spinnerAmount > 0.002 || badge > 0.002;

  @override
  bool operator ==(Object other) =>
      other is StageLook &&
      other.bird == bird &&
      other.ring == ring &&
      other.ringGap == ringGap &&
      other.spinner == spinner &&
      other.spinnerAmount == spinnerAmount &&
      other.spinnerTurns == spinnerTurns &&
      other.retry == retry &&
      other.fire == fire &&
      other.progress == progress &&
      other.lit == lit &&
      other.badge == badge &&
      other.badgeColor == badgeColor;

  @override
  int get hashCode => Object.hash(
    bird,
    ring,
    ringGap,
    spinner,
    spinnerAmount,
    spinnerTurns,
    retry,
    fire,
    progress,
    lit,
    badge,
    badgeColor,
  );

  static const _ash = Color(0xFF6B5E57);
  static const _soot = Color(0xFF3B2D27);
  static const _amber = Color(0xFFF2B134);
  static const _red = Color(0xFFE5483B);

  /// [step] is the stage of the connection being made, or the one it broke at.
  static StageLook of(
    Phase phase, {
    bool lostContact = false,
    bool recovering = false,
    ConnectStep? step,
  }) {
    switch (phase) {
      case Phase.idle:
        return const StageLook(
          bird: _ash,
          ring: _soot,
          ringGap: 0,
          spinner: _amber,
          spinnerAmount: 0,
          spinnerTurns: 0.8,
          retry: 0,
          fire: FireLook.out,
        );
      case Phase.connecting:
        return StageLook(
          bird: _amber,
          ring: const Color(0x47F2B134),
          ringGap: 0,
          spinner: const Color(0xFFFFC857),
          spinnerAmount: 0,
          spinnerTurns: 0.8,
          retry: 0,
          progress: (step ?? ConnectStep.settings).index.toDouble(),
          lit: 1,
          badge: 1,
          badgeColor: _amber,
          fire: const FireLook(
            color: Color(0xFFFFB347),
            level: 0.5,
            pulse: 0.5,
            period: 1.0,
            flicker: 0.6,
            embers: 0.35,
          ),
        );
      case Phase.connected:
        // Getting the connection back after sleep or a change of network: still
        // connected, so the bird keeps its colour, and the arc runs round the
        // ring as it does while connecting, to say that work is going on.
        if (recovering) {
          return const StageLook(
            bird: Ember.orange,
            ring: Color(0x47F2B134),
            ringGap: 0,
            spinner: Color(0xFFFFC857),
            spinnerAmount: 1,
            spinnerTurns: 1.1,
            retry: 0,
            progress: 4,
            badgeColor: Ember.orange,
            fire: FireLook(
              color: Color(0xFFFFB347),
              level: 0.6,
              pulse: 0.45,
              period: 1.2,
              flicker: 0.4,
              embers: 0.4,
            ),
          );
        }
        if (lostContact) {
          return const StageLook(
            bird: Color(0xFFB8861F),
            ring: _amber,
            ringGap: 0.45,
            spinner: _amber,
            spinnerAmount: 0,
            spinnerTurns: 0.8,
            retry: 0,
            fire: FireLook(
              color: Color(0xFFFFB347),
              level: 0.4,
              pulse: 0.25,
              period: 2.0,
              flicker: 0.9,
              embers: 0.12,
            ),
          );
        }
        return const StageLook(
          bird: Ember.orange,
          ring: Ember.orange,
          ringGap: 0,
          spinner: Ember.gold,
          spinnerAmount: 0,
          spinnerTurns: 0.8,
          retry: 0,
          progress: 4,
          lit: 1,
          badgeColor: Ember.orange,
          fire: FireLook(
            color: Ember.orange,
            level: 0.9,
            pulse: 0.4,
            period: 3.4,
            flicker: 0.1,
            embers: 0.55,
          ),
        );
      case Phase.switching:
        return const StageLook(
          bird: Ember.orange,
          ring: Ember.orange,
          ringGap: 0,
          spinner: Ember.gold,
          spinnerAmount: 1,
          spinnerTurns: 1.1,
          retry: 0,
          progress: 4,
          badgeColor: Ember.orange,
          fire: FireLook(
            color: Ember.orange,
            level: 0.95,
            pulse: 0.5,
            period: 0.9,
            flicker: 0.25,
            embers: 0.85,
          ),
        );
      case Phase.disconnecting:
        return const StageLook(
          bird: Color(0xFF8A6F5E),
          ring: _soot,
          ringGap: 0,
          spinner: Color(0xFFCC7A3A),
          spinnerAmount: 0.7,
          spinnerTurns: -0.6,
          retry: 0,
          progress: 4,
          badgeColor: Ember.orange,
          fire: FireLook(
            color: Color(0xFFCC6A2A),
            level: 0.22,
            pulse: 0.15,
            period: 1.5,
            flicker: 0.5,
          ),
        );
      case Phase.failed:
        return StageLook(
          bird: _red,
          ring: const Color(0x40FF6B5E),
          ringGap: 0,
          spinner: Ember.alarm,
          spinnerAmount: 0,
          spinnerTurns: 0.8,
          retry: 1,
          // The disc stays where it broke. Without that, there is no disc.
          progress: (step ?? ConnectStep.settings).index.toDouble(),
          badge: step == null ? 0 : 1,
          badgeColor: Ember.alarm,
          fire: const FireLook(
            color: Color(0xFFE5322B),
            level: 0.55,
            pulse: 0.85,
            period: 2.2,
            flicker: 0.05,
            heartbeat: 1,
          ),
        );
    }
  }

  static StageLook lerp(StageLook a, StageLook b, double t) => StageLook(
    bird: Color.lerp(a.bird, b.bird, t)!,
    ring: Color.lerp(a.ring, b.ring, t)!,
    ringGap: lerpDouble(a.ringGap, b.ringGap, t)!,
    spinner: Color.lerp(a.spinner, b.spinner, t)!,
    spinnerAmount: lerpDouble(a.spinnerAmount, b.spinnerAmount, t)!,
    // The direction is the one it is going to, so that it does not slow down,
    // stop and turn round on the way.
    spinnerTurns: t < 0.5 ? a.spinnerTurns : b.spinnerTurns,
    retry: lerpDouble(a.retry, b.retry, t)!,
    fire: FireLook.lerp(a.fire, b.fire, t),
    progress: lerpDouble(a.progress, b.progress, t)!,
    lit: lerpDouble(a.lit, b.lit, t)!,
    badge: lerpDouble(a.badge, b.badge, t)!,
    badgeColor: Color.lerp(a.badgeColor, b.badgeColor, t)!,
  );
}

class StageLookTween extends Tween<StageLook> {
  StageLookTween({super.begin, super.end});

  @override
  StageLook lerp(double t) => StageLook.lerp(begin!, end!, t);
}

/// The button, centred in a square of [size], and the light of the fire round it.
/// The light reaches well beyond the square and does not take part in layout.
class RebirthStage extends StatefulWidget {
  const RebirthStage({
    super.key,
    required this.phase,
    required this.onPressed,
    required this.size,
    this.lostContact = false,
    this.recovering = false,
    this.step,
  });

  final Phase phase;
  final bool lostContact;

  /// Connected, and the connection is being brought back.
  final bool recovering;

  /// The stage of the connection being made, or the one it failed at.
  final ConnectStep? step;
  final VoidCallback onPressed;

  /// The width of the button.
  final double size;

  @override
  State<RebirthStage> createState() => _RebirthStageState();
}

class _RebirthStageState extends State<RebirthStage>
    with SingleTickerProviderStateMixin {
  /// Seconds since the clock started. Everything that moves reads it.
  final _clock = ValueNotifier<double>(0);
  late final Ticker _ticker = createTicker(
    (elapsed) => _clock.value = elapsed.inMicroseconds / 1e6,
  );
  bool _hover = false;

  // What the disc showed last. The disc fades out after the connection is made or
  // has failed, and it keeps its sign while it does, though the status has
  // already forgotten the stage.
  ConnectStep _lastStep = ConnectStep.settings;
  bool _broken = false;

  @override
  void dispose() {
    _ticker.dispose();
    _clock.dispose();
    super.dispose();
  }

  /// The clock runs while something moves, and stops when nothing does.
  void _sync(bool needed) {
    if (needed && !_ticker.isActive) {
      _ticker.start();
    } else if (!needed && _ticker.isActive) {
      _ticker.stop();
    }
  }

  bool get _pressable => widget.phase != Phase.disconnecting;

  String get _label => switch (widget.phase) {
    Phase.connected => 'Отключить',
    Phase.idle || Phase.failed => 'Подключить',
    Phase.connecting || Phase.switching => 'Отменить',
    Phase.disconnecting => 'Отключаюсь',
  };

  @override
  Widget build(BuildContext context) {
    _lastStep = widget.step ?? _lastStep;
    if (widget.phase == Phase.failed) {
      _broken = true;
    } else if (widget.phase == Phase.connecting) {
      _broken = false;
    }
    final target = StageLook.of(
      widget.phase,
      lostContact: widget.lostContact,
      recovering: widget.recovering,
      step: widget.step,
    );
    final reduceMotion = MediaQuery.maybeDisableAnimationsOf(context) ?? false;
    final d = widget.size;

    return Semantics(
      button: true,
      label: _label,
      child: TweenAnimationBuilder<StageLook>(
        tween: StageLookTween(begin: target, end: target),
        duration: const Duration(milliseconds: 600),
        curve: Curves.easeInOut,
        builder: (context, look, _) {
          _sync(look.animated && !reduceMotion);
          // A person who asked for no motion gets the fire at its brightest, still.
          double time(double t) => reduceMotion ? look.fire.period * 0.5 : t;

          return SizedBox(
            width: d,
            height: d,
            child: Stack(
              clipBehavior: Clip.none,
              alignment: Alignment.center,
              children: [
                // The light, under everything and reaching far.
                Positioned.fill(
                  child: IgnorePointer(
                    child: ValueListenableBuilder<double>(
                      valueListenable: _clock,
                      builder: (context, t, _) => CustomPaint(
                        painter: FirePainter(
                          fire: look.fire,
                          t: time(t),
                          radius: d / 2,
                        ),
                      ),
                    ),
                  ),
                ),
                MouseRegion(
                  cursor: _pressable
                      ? SystemMouseCursors.click
                      : SystemMouseCursors.basic,
                  onEnter: (_) => setState(() => _hover = true),
                  onExit: (_) => setState(() => _hover = false),
                  child: AnimatedScale(
                    scale: _hover && _pressable ? 1.03 : 1,
                    duration: const Duration(milliseconds: 160),
                    curve: Curves.easeOut,
                    child: SizedBox(
                      width: d,
                      height: d,
                      child: Stack(
                        alignment: Alignment.center,
                        // The disc on the rim reaches past the button.
                        clipBehavior: Clip.none,
                        children: [
                          DecoratedBox(
                            decoration: const BoxDecoration(
                              shape: BoxShape.circle,
                              color: Ember.surface,
                            ),
                            child: SizedBox(width: d, height: d),
                          ),
                          IgnorePointer(
                            child: ValueListenableBuilder<double>(
                              valueListenable: _clock,
                              builder: (context, t, _) => CustomPaint(
                                size: Size(d, d),
                                painter: RingPainter(look: look, t: time(t)),
                              ),
                            ),
                          ),
                          IgnorePointer(
                            child: RebirthMark(
                              size: d * 0.56,
                              colors: RebirthColors.solid(look.bird),
                            ),
                          ),
                          if (look.badge > 0.01)
                            IgnorePointer(
                              child: StageBadge(
                                diameter: d,
                                progress: look.progress,
                                amount: look.badge,
                                color: look.badgeColor,
                                step: _lastStep,
                                broken: _broken,
                                clock: _clock,
                                time: time,
                              ),
                            ),
                          Material(
                            key: const ValueKey('power-button'),
                            color: Colors.transparent,
                            shape: const CircleBorder(),
                            child: InkWell(
                              customBorder: const CircleBorder(),
                              splashColor: look.fire.color.withValues(
                                alpha: 0.16,
                              ),
                              highlightColor: look.fire.color.withValues(
                                alpha: 0.08,
                              ),
                              onTap: _pressable ? widget.onPressed : null,
                              child: SizedBox(width: d, height: d),
                            ),
                          ),
                        ],
                      ),
                    ),
                  ),
                ),
              ],
            ),
          );
        },
      ),
    );
  }
}

/// Deterministic scatter in 0..1: the same spark gets the same number every time.
double _scatter(int index, int salt) {
  final x = math.sin(index * 12.9898 + salt * 78.233) * 43758.5453;
  return x - x.floorToDouble();
}

/// The light of the fire: three translucent circles, one outside the other, each
/// a little behind the one inside in its breathing, so that the light seems to
/// travel outwards; and sparks that lift off and fade. All flat.
class FirePainter extends CustomPainter {
  const FirePainter({
    required this.fire,
    required this.t,
    required this.radius,
  });

  final FireLook fire;
  final double t;

  /// The radius of the button the fire is under.
  final double radius;

  static const maxEmbers = 16;

  /// Size (in radii of the button) and strength of each circle of light.
  static const _layers = [(1.12, 0.24), (1.38, 0.13), (1.64, 0.06)];

  @override
  void paint(Canvas canvas, Size size) {
    if (fire.level <= 0) {
      return;
    }
    final r = radius;
    final c = size.center(Offset.zero);

    var brightest = 0.0;
    for (var i = 0; i < _layers.length; i++) {
      final (scale, strength) = _layers[i];
      // Each circle a little behind the last.
      final k = fireIntensity(fire, t - i * 0.12 * fire.period);
      brightest = math.max(brightest, k);
      final a = (strength * k).clamp(0.0, 1.0);
      if (a <= 0.004) {
        continue;
      }
      canvas.drawCircle(
        c,
        r * scale * (0.97 + 0.05 * k),
        Paint()..color = fire.color.withValues(alpha: a),
      );
    }
    _sparks(canvas, c, r, brightest);
  }

  void _sparks(Canvas canvas, Offset c, double r, double k) {
    final count = (maxEmbers * fire.embers).round();
    for (var i = 0; i < count; i++) {
      final life = 2.4 + 2.0 * _scatter(i, 1);
      final phase = ((t + _scatter(i, 2) * life) / life) % 1.0;
      final x =
          c.dx +
          (_scatter(i, 3) - 0.5) * r * 1.7 +
          math.sin(t * 1.3 + i) * r * 0.08 * phase;
      final y = c.dy + r * 0.55 - phase * r * 2.3;
      final fade = math.pow(math.sin(math.pi * phase), 1.2).toDouble();
      final alpha = (k * fade * (0.5 + 0.5 * _scatter(i, 4))).clamp(0.0, 1.0);
      if (alpha <= 0.02) {
        continue;
      }
      final size = (1.5 + 1.8 * _scatter(i, 5)) * (1 - 0.5 * phase) * (r / 90);
      canvas.drawCircle(
        Offset(x, y),
        math.max(size, 0.9),
        Paint()
          ..color = Color.lerp(
            fire.color,
            Ember.gold,
            1 - phase,
          )!.withValues(alpha: alpha),
      );
    }
  }

  @override
  bool shouldRepaint(FirePainter old) =>
      old.t != t || old.radius != radius || old.fire != fire;
}

/// The ring round the button: whole, broken into dashes, with a spark running
/// round it, or open with an arrow.
class RingPainter extends CustomPainter {
  const RingPainter({required this.look, required this.t});

  final StageLook look;
  final double t;

  static const _width = 3.0;

  @override
  void paint(Canvas canvas, Size size) {
    final centre = size.center(Offset.zero);
    final radius = size.width / 2 - _width / 2;
    final rect = Rect.fromCircle(center: centre, radius: radius);

    _base(canvas, rect);
    if (look.lit > 0.01) {
      _lit(canvas, rect);
    }
    if (look.spinnerAmount > 0.01) {
      _spinner(canvas, centre, rect);
    }
    if (look.retry > 0.01) {
      _retry(canvas, centre, radius, rect);
    }
  }

  Paint _stroke(Color color) => Paint()
    ..style = PaintingStyle.stroke
    ..strokeWidth = _width
    ..strokeCap = StrokeCap.round
    ..strokeJoin = StrokeJoin.round
    ..color = color;

  void _base(Canvas canvas, Rect rect) {
    if (look.ring.a <= 0.01) {
      return;
    }
    final paint = _stroke(look.ring)..strokeCap = StrokeCap.butt;
    if (look.ringGap < 0.02) {
      canvas.drawCircle(rect.center, rect.width / 2, paint);
      return;
    }
    // Dashes: the ring is cut into pieces and a part of each is taken away.
    const pieces = 28;
    final step = 2 * math.pi / pieces;
    for (var i = 0; i < pieces; i++) {
      canvas.drawArc(rect, i * step, step * (1 - look.ringGap), false, paint);
    }
  }

  /// The part of the rim the stages have got through: from the top, clockwise,
  /// as far as the disc.
  void _lit(Canvas canvas, Rect rect) {
    final sweep = look.progress.clamp(0.0, 4.0) * math.pi / 2;
    if (sweep < 0.02) {
      return;
    }
    final paint = _stroke(
      look.badgeColor.withValues(alpha: look.lit.clamp(0.0, 1.0)),
    )..strokeCap = StrokeCap.butt;
    canvas.drawArc(rect, -math.pi / 2, sweep, false, paint);
  }

  /// A short arc that runs round the ring.
  void _spinner(Canvas canvas, Offset centre, Rect rect) {
    final color = look.spinner.withValues(
      alpha: look.spinnerAmount.clamp(0.0, 1.0),
    );
    final angle = t * look.spinnerTurns * 2 * math.pi;
    canvas.drawArc(rect, angle, 1.5, false, _stroke(color));
  }

  /// An open ring with an arrowhead: the sign for "again".
  void _retry(Canvas canvas, Offset centre, double radius, Rect rect) {
    final paint = _stroke(
      Ember.alarm.withValues(alpha: look.retry.clamp(0, 1)),
    );
    const start = -math.pi / 2 + 0.55;
    const sweep = 2 * math.pi - 1.1;
    canvas.drawArc(rect, start, sweep, false, paint);

    // The arrowhead at the end, pointing the way the ring runs (clockwise).
    const end = start + sweep;
    final tip = centre + Offset(math.cos(end), math.sin(end)) * radius;
    final along = Offset(-math.sin(end), math.cos(end));
    Offset wing(double turn) {
      final c = math.cos(turn);
      final s = math.sin(turn);
      // `along` turned by [turn], then reversed, 11 long.
      return tip -
          Offset(along.dx * c - along.dy * s, along.dx * s + along.dy * c) * 11;
    }

    canvas.drawPath(
      Path()
        ..moveTo(wing(0.55).dx, wing(0.55).dy)
        ..lineTo(tip.dx, tip.dy)
        ..lineTo(wing(-0.55).dx, wing(-0.55).dy),
      paint,
    );
  }

  @override
  bool shouldRepaint(RingPainter old) => old.t != t || old.look != look;
}
