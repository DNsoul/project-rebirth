import 'dart:async';

import 'package:flutter/material.dart';

import '../core/controller.dart';
import '../core/regions.dart';
import '../core/updater.dart';
import 'rebirth_mark.dart';
import 'theme.dart';
import 'update_banner.dart';

export '../core/regions.dart' show regionLabel, regionNames;

/// One button, and what it is doing.
class HomePage extends StatelessWidget {
  const HomePage({
    super.key,
    required this.controller,
    this.hint = '',
    this.updates,
  });

  final ConnectionView controller;

  /// A line of advice for this platform, under the standing one.
  final String hint;

  /// Where a newer version is offered, in the place of the advice. Without it
  /// there is no such place.
  final UpdateView? updates;

  @override
  Widget build(BuildContext context) {
    final advice = Text(
      'Серверы меняются сами — настраивать ничего не нужно.'
      '${hint.isEmpty ? '' : '\n$hint'}',
      textAlign: TextAlign.center,
      style: Theme.of(context).textTheme.bodySmall?.copyWith(
        color: Theme.of(context).colorScheme.onSurfaceVariant,
      ),
    );
    return Scaffold(
      body: SafeArea(
        child: StreamBuilder<AppStatus>(
          initialData: controller.status,
          stream: controller.statusStream,
          builder: (context, snapshot) {
            final status = snapshot.data!;
            // On a screen that is tall enough the spacers spread the content out;
            // on a short one (a small window, a phone on its side) it scrolls
            // instead of overflowing.
            return LayoutBuilder(
              builder: (context, box) => SingleChildScrollView(
                child: ConstrainedBox(
                  constraints: BoxConstraints(minHeight: box.maxHeight),
                  child: IntrinsicHeight(
                    child: Padding(
                      padding: const EdgeInsets.symmetric(
                        horizontal: 24,
                        vertical: 20,
                      ),
                      child: Column(
                        children: [
                          const _Wordmark(),
                          const Spacer(flex: 2),
                          PowerButton(
                            phase: status.phase,
                            lostContact: status.lostContact,
                            onPressed: () => _press(status),
                          ),
                          const SizedBox(height: 28),
                          _Headline(status: status),
                          const SizedBox(height: 10),
                          // A fixed height, so the button stays where it is however
                          // much there is to say under it.
                          SizedBox(
                            height: 150,
                            child: Align(
                              alignment: Alignment.topCenter,
                              child: _Details(
                                status: status,
                                onRetry: controller.connect,
                              ),
                            ),
                          ),
                          const Spacer(flex: 2),
                          if (updates == null)
                            advice
                          else
                            // A fixed height: the offer takes the place of the advice and
                            // nothing above it moves when it appears.
                            SizedBox(
                              height: 132,
                              child: Align(
                                alignment: Alignment.bottomCenter,
                                child: UpdateBanner(
                                  updates: updates!,
                                  fallback: advice,
                                ),
                              ),
                            ),
                        ],
                      ),
                    ),
                  ),
                ),
              ),
            );
          },
        ),
      ),
    );
  }

  void _press(AppStatus status) {
    switch (status.phase) {
      case Phase.idle:
      case Phase.failed:
        unawaited(controller.connect());
      case Phase.connected:
        unawaited(controller.disconnect());
      case Phase.connecting:
      case Phase.switching:
        // The button turns into "cancel": nobody should have to sit through a
        // connection they no longer want.
        unawaited(controller.cancel());
      case Phase.disconnecting:
        break; // already on its way; a second press has nothing to add
    }
  }
}

class PowerButton extends StatelessWidget {
  const PowerButton({
    super.key,
    required this.phase,
    required this.lostContact,
    required this.onPressed,
  });

  final Phase phase;
  final bool lostContact;
  final VoidCallback onPressed;

  static const _size = 196.0;

  @override
  Widget build(BuildContext context) {
    final busy =
        phase == Phase.connecting ||
        phase == Phase.switching ||
        phase == Phase.disconnecting;
    // Everything in progress can be called off except taking the tunnel down,
    // which is already what the person asked for.
    final pressable = phase != Phase.disconnecting;

    // What the state looks like: the colour of the ring, how far the mark glows,
    // and the colours of the mark itself. The bird is lit when the tunnel is up,
    // ash when it is off, amber while something is going on, red when it failed.
    final Color ring;
    final RebirthColors mark;
    final double glow;
    if (phase == Phase.failed) {
      ring = Ember.alarm;
      mark = const RebirthColors(
        Color(0xFFFF9A8E),
        Color(0xFFE5483B),
        Color(0xFF9A1E18),
      );
      glow = 0.3;
    } else if (phase == Phase.connected && !lostContact) {
      ring = Ember.orange;
      mark = RebirthColors.flame;
      glow = 1;
    } else if (phase == Phase.connected || phase == Phase.switching) {
      ring = Ember.amber;
      mark = const RebirthColors(
        Color(0xFFFFE08A),
        Color(0xFFF2B134),
        Color(0xFFB87A12),
      );
      glow = 0.5;
    } else if (busy) {
      ring = Ember.amber;
      mark = RebirthColors.flame;
      glow = 0.35;
    } else {
      ring = Ember.outline;
      mark = RebirthColors.solid(Ember.ash);
      glow = 0;
    }

    return Semantics(
      button: true,
      label: switch (phase) {
        Phase.connected => 'Отключить',
        Phase.idle || Phase.failed => 'Подключить',
        Phase.connecting || Phase.switching => 'Отменить',
        Phase.disconnecting => 'Отключаюсь',
      },
      child: SizedBox(
        width: _size,
        height: _size,
        child: Stack(
          alignment: Alignment.center,
          children: [
            // The glow behind the disc.
            AnimatedContainer(
              duration: const Duration(milliseconds: 400),
              width: _size - 28,
              height: _size - 28,
              decoration: BoxDecoration(
                shape: BoxShape.circle,
                boxShadow: [
                  if (glow > 0)
                    BoxShadow(
                      color: ring.withValues(alpha: 0.35 * glow),
                      blurRadius: 46,
                      spreadRadius: 6,
                    ),
                ],
              ),
            ),
            if (busy)
              const SizedBox(
                width: _size,
                height: _size,
                child: CircularProgressIndicator(strokeWidth: 3),
              ),
            // The disc with its ring.
            AnimatedContainer(
              duration: const Duration(milliseconds: 400),
              width: _size - 20,
              height: _size - 20,
              decoration: BoxDecoration(
                shape: BoxShape.circle,
                color: Ember.surface,
                border: Border.all(color: ring, width: 2.5),
              ),
              child: Material(
                key: const ValueKey('power-button'),
                color: Colors.transparent,
                shape: const CircleBorder(),
                child: InkWell(
                  customBorder: const CircleBorder(),
                  splashColor: ring.withValues(alpha: 0.18),
                  highlightColor: ring.withValues(alpha: 0.08),
                  onTap: pressable ? onPressed : null,
                  child: Center(
                    child: RebirthMark(
                      size: 104,
                      colors: mark,
                      glow: glow * 0.6,
                    ),
                  ),
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

/// The name at the top of the screen, with the mark beside it.
class _Wordmark extends StatelessWidget {
  const _Wordmark();

  @override
  Widget build(BuildContext context) {
    return Row(
      mainAxisAlignment: MainAxisAlignment.center,
      children: [
        const RebirthMark(size: 26),
        const SizedBox(width: 10),
        ShaderMask(
          shaderCallback: (rect) => const LinearGradient(
            colors: [Ember.gold, Ember.orange],
          ).createShader(rect),
          child: Text(
            'REBIRTH',
            key: const ValueKey('wordmark'),
            style: Theme.of(context).textTheme.titleMedium?.copyWith(
              color: Colors.white,
              fontWeight: FontWeight.w700,
              letterSpacing: 6,
            ),
          ),
        ),
      ],
    );
  }
}

class _Headline extends StatelessWidget {
  const _Headline({required this.status});

  final AppStatus status;

  @override
  Widget build(BuildContext context) {
    final text = switch (status.phase) {
      Phase.idle => 'Отключено',
      Phase.connecting => 'Подключаюсь…',
      Phase.connected =>
        status.lostContact ? 'Нет ответа от сервера' : 'Подключено',
      Phase.switching => 'Меняю сервер…',
      Phase.disconnecting => 'Отключаюсь…',
      Phase.failed => 'Не получилось',
    };
    final color = switch (status.phase) {
      Phase.connected => status.lostContact ? Ember.amber : Ember.gold,
      Phase.failed => Ember.alarm,
      _ => Ember.text,
    };
    return Text(
      text,
      key: const ValueKey('headline'),
      style: Theme.of(context).textTheme.headlineMedium?.copyWith(
        color: color,
        fontWeight: FontWeight.w600,
      ),
    );
  }
}

class _Details extends StatelessWidget {
  const _Details({required this.status, required this.onRetry});

  final AppStatus status;
  final Future<void> Function() onRetry;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final style = Theme.of(context).textTheme.bodyMedium;
    final lines = <Widget>[];

    if (status.phase == Phase.idle) {
      lines.add(
        Text(
          'Нажмите на феникса, чтобы подключиться.',
          key: const ValueKey('tap-hint'),
          textAlign: TextAlign.center,
          style: style?.copyWith(color: scheme.onSurfaceVariant),
        ),
      );
    }
    if (status.isUp && status.region.isNotEmpty) {
      lines.add(Text('Сервер: ${regionLabel(status.region)}', style: style));
    }
    if (status.phase == Phase.connected && status.since != null) {
      lines.add(Uptime(since: status.since!, style: style));
    }
    if (status.lostContact) {
      lines.add(
        Text(
          'Ищу замену — она подключится сама.',
          style: style?.copyWith(color: scheme.tertiary),
        ),
      );
    }
    if (status.message.isNotEmpty &&
        status.phase != Phase.connected &&
        status.phase != Phase.idle) {
      lines.add(
        Text(
          status.message,
          key: const ValueKey('message'),
          textAlign: TextAlign.center,
          style: style?.copyWith(
            color: status.phase == Phase.failed ? scheme.error : null,
          ),
        ),
      );
    }
    if (status.note.isNotEmpty) {
      lines.add(
        Text(
          status.note,
          key: const ValueKey('note'),
          textAlign: TextAlign.center,
          style: style?.copyWith(color: scheme.onSurfaceVariant),
        ),
      );
    }
    if (status.canBeCancelled) {
      lines.add(
        Text(
          'Нажмите кнопку, чтобы отменить.',
          key: const ValueKey('cancel-hint'),
          style: Theme.of(
            context,
          ).textTheme.bodySmall?.copyWith(color: scheme.onSurfaceVariant),
        ),
      );
    }
    if (status.phase == Phase.failed) {
      lines.add(
        Padding(
          padding: const EdgeInsets.only(top: 8),
          child: FilledButton.tonal(
            onPressed: () => unawaited(onRetry()),
            child: const Text('Повторить'),
          ),
        ),
      );
    }

    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        for (final w in lines)
          Padding(padding: const EdgeInsets.symmetric(vertical: 2), child: w),
      ],
    );
  }
}

/// How long the connection has been up, counting.
class Uptime extends StatefulWidget {
  const Uptime({super.key, required this.since, this.style});

  final DateTime since;
  final TextStyle? style;

  @override
  State<Uptime> createState() => _UptimeState();
}

class _UptimeState extends State<Uptime> {
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _timer = Timer.periodic(const Duration(seconds: 1), (_) {
      if (mounted) setState(() {});
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Text(
      'В сети ${formatDuration(DateTime.now().difference(widget.since))}',
      style: widget.style,
    );
  }
}

String formatDuration(Duration d) {
  if (d.isNegative) d = Duration.zero;
  String two(int n) => n.toString().padLeft(2, '0');
  final h = d.inHours;
  final m = d.inMinutes.remainder(60);
  final s = d.inSeconds.remainder(60);
  return h > 0 ? '$h:${two(m)}:${two(s)}' : '${two(m)}:${two(s)}';
}
