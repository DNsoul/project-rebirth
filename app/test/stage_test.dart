import 'dart:math' as math;

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/ui/error_panel.dart';
import 'package:awg_client/ui/stage.dart';
import 'package:awg_client/ui/stage_badge.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

/// The brightness of the fire over [seconds], sampled every 20 ms.
List<double> sample(FireLook f, double seconds) => [
  for (var t = 0.0; t < seconds; t += 0.02) fireIntensity(f, t),
];

Widget stage(
  Phase phase, {
  bool lost = false,
  double size = 180,
  ConnectStep? step,
}) => MaterialApp(
  theme: appTheme(),
  home: Scaffold(
    backgroundColor: Ember.background,
    body: Center(
      child: RebirthStage(
        phase: phase,
        lostContact: lost,
        step: step,
        size: size,
        onPressed: () {},
      ),
    ),
  ),
);

void main() {
  group('the fire', () {
    test('is dark when it is out', () {
      expect(sample(FireLook.out, 5).every((v) => v == 0), isTrue);
    });

    test('breathes slowly when connected, between a low and a high', () {
      final look = StageLook.of(Phase.connected).fire;
      final v = sample(look, look.period * 2);
      final lo = v.reduce(math.min);
      final hi = v.reduce(math.max);
      expect(hi, greaterThan(0.7));
      expect(lo, greaterThan(0.2), reason: 'it dims, but does not go out');
      expect(hi - lo, greaterThan(0.25), reason: 'the breathing can be seen');
      expect(hi, lessThan(1.3));
    });

    test('flares quicker while connecting or moving than when it burns', () {
      double swing(Phase p) {
        final f = StageLook.of(p).fire;
        return f.period;
      }

      expect(swing(Phase.connecting), lessThan(swing(Phase.connected)));
      expect(swing(Phase.switching), lessThan(swing(Phase.connected)));
    });

    test('beats twice and rests when something has failed', () {
      final look = StageLook.of(Phase.failed).fire;
      final v = sample(look, look.period);
      final hi = v.reduce(math.max);
      // Count the beats: separate rises above half the peak.
      var beats = 0;
      var up = false;
      for (final x in v) {
        if (!up && x > hi * 0.6) {
          beats++;
          up = true;
        } else if (up && x < hi * 0.3) {
          up = false;
        }
      }
      expect(beats, 2);
      expect(
        v.where((x) => x < hi * 0.2).length,
        greaterThan(v.length ~/ 3),
        reason: 'and then a rest',
      );
    });

    test('sputters when the server is silent: uneven and weak', () {
      final silent = StageLook.of(Phase.connected, lostContact: true).fire;
      final burning = StageLook.of(Phase.connected).fire;
      expect(silent.flicker, greaterThan(burning.flicker * 3));
      expect(silent.level, lessThan(burning.level));
      expect(silent.embers, lessThan(burning.embers));
    });

    test('is the same at the same moment', () {
      final look = StageLook.of(Phase.connected).fire;
      expect(fireIntensity(look, 1.234), fireIntensity(look, 1.234));
    });
  });

  group('bringing the connection back', () {
    final back = StageLook.of(
      Phase.connected,
      lostContact: true,
      recovering: true,
    );
    final silent = StageLook.of(Phase.connected, lostContact: true);
    final connected = StageLook.of(Phase.connected);

    test(
      'keeps the colour of a connection and runs the arc round the ring',
      () {
        expect(back.bird, connected.bird);
        expect(back.spinnerAmount, 1);
        expect(silent.spinnerAmount, 0);
      },
    );

    test('does not look like a server that does not answer', () {
      expect(back.bird, isNot(silent.bird));
      expect(back.ringGap, 0, reason: 'the broken ring is for silence only');
      expect(silent.ringGap, greaterThan(0));
    });
  });

  group('the look of each state', () {
    test('off has no light and nothing that moves', () {
      final off = StageLook.of(Phase.idle);
      expect(off.fire.level, 0);
      expect(off.animated, isFalse);
    });

    test('every other state has light, or a spark, and moves', () {
      for (final p in [
        Phase.connecting,
        Phase.connected,
        Phase.switching,
        Phase.disconnecting,
        Phase.failed,
      ]) {
        expect(StageLook.of(p).animated, isTrue, reason: '$p');
      }
    });

    test('a spark runs round the ring while a move is being made', () {
      expect(StageLook.of(Phase.switching).spinnerAmount, greaterThan(0.5));
      expect(StageLook.of(Phase.connected).spinnerAmount, 0);
      expect(StageLook.of(Phase.idle).spinnerAmount, 0);
    });

    test(
      'while connecting the spark is the disc, so the ring has no other',
      () {
        final look = StageLook.of(Phase.connecting);
        expect(look.spinnerAmount, 0);
        expect(look.badge, 1);
        expect(look.lit, 1);
      },
    );

    test('the disc is only there while connecting, and where it broke', () {
      for (final p in Phase.values) {
        final look = StageLook.of(p, step: ConnectStep.tunnel);
        final shown = p == Phase.connecting || p == Phase.failed;
        expect(look.badge, shown ? 1 : 0, reason: '$p');
      }
      expect(
        StageLook.of(Phase.failed).badge,
        0,
        reason: 'nothing to say where it broke',
      );
    });

    test('the disc sits a quarter of the rim further on at each stage', () {
      for (final step in ConnectStep.values) {
        expect(
          StageLook.of(Phase.connecting, step: step).progress,
          step.index.toDouble(),
        );
        expect(
          StageLook.of(Phase.failed, step: step).progress,
          step.index.toDouble(),
        );
      }
      expect(StageLook.of(Phase.connecting).progress, 0);
      expect(
        StageLook.of(Phase.connected).progress,
        4,
        reason: 'when it is up the whole rim is lit',
      );
    });

    test('after a failure the rim is not lit: the arrow takes it', () {
      expect(StageLook.of(Phase.failed, step: ConnectStep.server).lit, 0);
      expect(StageLook.of(Phase.connecting).lit, 1);
    });

    test('a look is equal to a look made the same way, and to no other', () {
      expect(
        StageLook.of(Phase.connecting, step: ConnectStep.server),
        StageLook.of(Phase.connecting, step: ConnectStep.server),
      );
      expect(
        StageLook.of(Phase.connecting, step: ConnectStep.server).hashCode,
        StageLook.of(Phase.connecting, step: ConnectStep.server).hashCode,
      );
      expect(
        StageLook.of(Phase.connecting, step: ConnectStep.server),
        isNot(StageLook.of(Phase.connecting, step: ConnectStep.tunnel)),
      );
      expect(
        StageLook.of(Phase.connected),
        isNot(StageLook.of(Phase.connected, lostContact: true)),
      );
    });

    test('the disc takes the places between when the look changes', () {
      final a = StageLook.of(Phase.connecting, step: ConnectStep.settings);
      final b = StageLook.of(Phase.connecting, step: ConnectStep.tunnel);
      expect(StageLook.lerp(a, b, 0.5).progress, closeTo(0.5, 1e-9));
      final failed = StageLook.of(Phase.failed, step: ConnectStep.tunnel);
      expect(StageLook.lerp(b, failed, 0.5).lit, closeTo(0.5, 1e-9));
    });

    test('going off runs the spark the other way', () {
      expect(StageLook.of(Phase.disconnecting).spinnerTurns, lessThan(0));
      expect(StageLook.of(Phase.connecting).spinnerTurns, greaterThan(0));
    });

    test('a failure has an arrow that says "again", and nothing else does', () {
      expect(StageLook.of(Phase.failed).retry, 1);
      for (final p in [Phase.idle, Phase.connecting, Phase.connected]) {
        expect(StageLook.of(p).retry, 0, reason: '$p');
      }
    });

    test('a silent server breaks the ring', () {
      expect(
        StageLook.of(Phase.connected, lostContact: true).ringGap,
        greaterThan(0.2),
      );
      expect(StageLook.of(Phase.connected).ringGap, 0);
    });

    test('the states can be told apart by the colour of the bird', () {
      final colours = {for (final p in Phase.values) p: StageLook.of(p).bird};
      expect(colours[Phase.idle], isNot(colours[Phase.connected]));
      expect(colours[Phase.failed], isNot(colours[Phase.connected]));
      expect(colours[Phase.connecting], isNot(colours[Phase.connected]));
      expect(
        StageLook.of(Phase.connected, lostContact: true).bird,
        isNot(colours[Phase.connected]),
      );
    });

    test('changing from one to another goes through the places between', () {
      final a = StageLook.of(Phase.idle);
      final b = StageLook.of(Phase.connected);
      final mid = StageLook.lerp(a, b, 0.5);
      expect(mid.fire.level, closeTo(b.fire.level / 2, 1e-9));
      expect(StageLook.lerp(a, b, 0).fire.level, 0);
      expect(StageLook.lerp(a, b, 1).fire.level, b.fire.level);
    });
  });

  group('the stage', () {
    testWidgets('stops its clock when nothing moves', (tester) async {
      await tester.pumpWidget(stage(Phase.idle));
      // Nothing is scheduled: settling returns at once instead of timing out.
      await tester.pumpAndSettle();
      expect(tester.binding.hasScheduledFrame, isFalse);
    });

    testWidgets('keeps the clock running while the fire burns', (tester) async {
      await tester.pumpWidget(stage(Phase.connected));
      await tester.pump(const Duration(milliseconds: 100));
      expect(tester.binding.hasScheduledFrame, isTrue);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('stops again when the fire has gone out', (tester) async {
      await tester.pumpWidget(stage(Phase.connected));
      await tester.pump(const Duration(milliseconds: 100));
      await tester.pumpWidget(stage(Phase.idle));
      // The fade takes a moment; after it nothing is scheduled any more.
      await tester.pumpAndSettle(const Duration(milliseconds: 50));
      expect(tester.binding.hasScheduledFrame, isFalse);
    });

    testWidgets('for a person who asked for no motion, nothing moves', (
      tester,
    ) async {
      await tester.pumpWidget(
        MediaQuery(
          data: const MediaQueryData(disableAnimations: true),
          child: stage(Phase.connected),
        ),
      );
      await tester.pumpAndSettle();
      expect(tester.binding.hasScheduledFrame, isFalse);
    });

    testWidgets('draws in every state and at every size without trouble', (
      tester,
    ) async {
      for (final size in const [110.0, 180.0, 250.0]) {
        for (final p in Phase.values) {
          await tester.pumpWidget(stage(p, size: size));
          await tester.pump(const Duration(milliseconds: 700));
          expect(tester.takeException(), isNull, reason: '$p at $size');
        }
        await tester.pumpWidget(stage(Phase.connected, lost: true, size: size));
        await tester.pump(const Duration(milliseconds: 700));
        expect(tester.takeException(), isNull);
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('draws every stage, and the failure at each, at every size', (
      tester,
    ) async {
      for (final size in const [110.0, 180.0, 250.0]) {
        for (final step in ConnectStep.values) {
          for (final p in [Phase.connecting, Phase.failed]) {
            await tester.pumpWidget(stage(p, size: size, step: step));
            await tester.pump(const Duration(milliseconds: 700));
            expect(tester.takeException(), isNull, reason: '$p $step $size');
          }
        }
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('shows the sign of the stage being done, and no other', (
      tester,
    ) async {
      for (final step in ConnectStep.values) {
        await tester.pumpWidget(stage(Phase.connecting, step: step));
        await tester.pump(const Duration(milliseconds: 700));
        for (final other in ConnectStep.values) {
          expect(
            find.byIcon(stepIcon(other)),
            other == step ? findsOneWidget : findsNothing,
            reason: 'at $step, looking for $other',
          );
        }
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('puts it at the top, right, bottom and left of the rim', (
      tester,
    ) async {
      const d = 200.0;
      // The signs that stay put as they move: the plug and the arrows slide a
      // little, which the tolerance allows for.
      final at = {
        ConnectStep.settings: const Offset(0, -1),
        ConnectStep.tunnel: const Offset(1, 0),
        ConnectStep.server: const Offset(0, 1),
        ConnectStep.traffic: const Offset(-1, 0),
      };
      for (final entry in at.entries) {
        await tester.pumpWidget(
          stage(Phase.connecting, size: d, step: entry.key),
        );
        await tester.pump(const Duration(milliseconds: 700));
        final button = tester.getCenter(
          find.byKey(const ValueKey('power-button')),
        );
        final sign = tester.getCenter(find.byIcon(stepIcon(entry.key)));
        final wanted = button + entry.value * (d / 2 - 1.5);
        expect((sign - wanted).distance, lessThan(8), reason: '${entry.key}');
      }
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('slides to the next place when a stage is done', (
      tester,
    ) async {
      await tester.pumpWidget(
        stage(Phase.connecting, size: 200, step: ConnectStep.settings),
      );
      await tester.pump(const Duration(milliseconds: 700));
      await tester.pumpWidget(
        stage(Phase.connecting, size: 200, step: ConnectStep.tunnel),
      );
      await tester.pump(const Duration(milliseconds: 150));
      final button = tester.getCenter(
        find.byKey(const ValueKey('power-button')),
      );
      final sign = tester.getCenter(find.byIcon(stepIcon(ConnectStep.tunnel)));
      // Part of the way: right of the top, but not yet at the right.
      expect(sign.dx, greaterThan(button.dx + 5));
      expect(sign.dx, lessThan(button.dx + 98));
      expect(sign.dy, lessThan(button.dy - 5));
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('shows a cross where it broke, and nothing when idle', (
      tester,
    ) async {
      await tester.pumpWidget(stage(Phase.failed, step: ConnectStep.server));
      await tester.pump(const Duration(milliseconds: 700));
      expect(find.byIcon(Icons.close_rounded), findsOneWidget);
      expect(find.byIcon(stepIcon(ConnectStep.server)), findsNothing);

      await tester.pumpWidget(stage(Phase.idle));
      await tester.pumpAndSettle(const Duration(milliseconds: 50));
      expect(find.byIcon(Icons.close_rounded), findsNothing);
      for (final step in ConnectStep.values) {
        expect(find.byIcon(stepIcon(step)), findsNothing);
      }
    });

    testWidgets('keeps its sign while it fades out after the connection', (
      tester,
    ) async {
      await tester.pumpWidget(
        stage(Phase.connecting, step: ConnectStep.traffic),
      );
      await tester.pump(const Duration(milliseconds: 700));
      await tester.pumpWidget(stage(Phase.connected));
      await tester.pump(const Duration(milliseconds: 200));
      expect(find.byIcon(stepIcon(ConnectStep.traffic)), findsOneWidget);
      expect(find.byIcon(stepIcon(ConnectStep.settings)), findsNothing);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('takes no more room than the button', (tester) async {
      await tester.pumpWidget(stage(Phase.connected, size: 180));
      await tester.pump(const Duration(milliseconds: 700));
      expect(
        tester.getSize(find.byKey(const ValueKey('power-button'))),
        const Size(180, 180),
      );
      await tester.pumpWidget(const SizedBox());
    });
  });

  group('the words of an error', () {
    test('are split into what happened and what to do', () {
      expect(splitMessage('Нет связи с GitHub. Проверьте интернет.'), (
        'Нет связи с GitHub.',
        'Проверьте интернет.',
      ));
    });

    test('keep every sentence after the first together', () {
      expect(
        splitMessage('Сбой. Повторите. Если не помогает, перезапустите.'),
        ('Сбой.', 'Повторите. Если не помогает, перезапустите.'),
      );
    });

    test('are left whole when there is one sentence', () {
      expect(splitMessage('Сервер не отвечает.'), ('Сервер не отвечает.', ''));
      expect(splitMessage('Без точки'), ('Без точки', ''));
    });

    test('cope with a line break and with nothing', () {
      expect(splitMessage('Не удалось подключиться.\nПовторите.'), (
        'Не удалось подключиться.',
        'Повторите.',
      ));
      expect(splitMessage('  '), ('', ''));
    });

    test('two notices of the same words are the same notice', () {
      expect(
        const Notice('a', Severity.error),
        const Notice('a', Severity.error),
      );
      expect(
        const Notice('a', Severity.error),
        isNot(const Notice('a', Severity.warning)),
      );
    });
  });
}
