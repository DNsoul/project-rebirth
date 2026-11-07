import 'dart:async';

import 'package:flutter/material.dart';

import '../core/updater.dart';
import 'theme.dart';

/// A newer version exists: one flat line along the top edge of the window. The
/// whole line is the button. It stays until the version is installed, and shows
/// nothing when there is nothing to offer.
///
/// It says only that, and how the download is going. If the update fails the
/// title turns red and the line offers to try again; what went wrong is in the
/// bar at the bottom, where every error is.
class UpdateBanner extends StatelessWidget {
  const UpdateBanner({super.key, required this.updates, this.topInset = 0});

  final UpdateView updates;

  /// The height of whatever the system draws over the top of the window (the
  /// status bar). The line runs under it and keeps its text clear.
  final double topInset;

  @override
  Widget build(BuildContext context) {
    return StreamBuilder<UpdateStatus?>(
      initialData: updates.status,
      stream: updates.statusStream,
      builder: (context, snapshot) {
        final s = snapshot.data;
        return AnimatedSwitcher(
          duration: const Duration(milliseconds: 220),
          transitionBuilder: (child, animation) => FadeTransition(
            opacity: animation,
            child: SizeTransition(
              sizeFactor: animation,
              axisAlignment: -1,
              child: child,
            ),
          ),
          child: s == null
              ? const SizedBox(
                  key: ValueKey('no-update'),
                  width: double.infinity,
                )
              : _Line(
                  key: const ValueKey('update-banner'),
                  status: s,
                  topInset: topInset,
                  onInstall: () => unawaited(updates.install()),
                ),
        );
      },
    );
  }
}

class _Line extends StatelessWidget {
  const _Line({
    super.key,
    required this.status,
    required this.topInset,
    required this.onInstall,
  });

  final UpdateStatus status;
  final double topInset;
  final VoidCallback onInstall;

  @override
  Widget build(BuildContext context) {
    final style = Theme.of(
      context,
    ).textTheme.bodyMedium!.copyWith(fontSize: 13, fontWeight: FontWeight.w600);
    final offer = status.offer;
    final phase = status.phase;
    final failed = phase == UpdatePhase.failed;
    final working =
        phase == UpdatePhase.downloading || phase == UpdatePhase.installing;

    final String title;
    switch (phase) {
      case UpdatePhase.available:
        title = offer.mandatory
            ? 'Обновите до ${offer.version}'
            : 'Версия ${offer.version}';
      case UpdatePhase.downloading:
        title = 'Скачиваю ${offer.version}';
      case UpdatePhase.installing:
        title = 'Запускаю установщик…';
      case UpdatePhase.failed:
        title = 'Сбой обновления';
    }
    final String? action = switch (phase) {
      UpdatePhase.available => 'Обновить',
      UpdatePhase.failed => 'Повторить',
      UpdatePhase.downloading => '${(status.progress * 100).floor()}%',
      UpdatePhase.installing => null,
    };

    return Material(
      color: Ember.surfaceHigh,
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          InkWell(
            key: const ValueKey('update-install'),
            onTap: working ? null : onInstall,
            child: Padding(
              padding: EdgeInsets.fromLTRB(14, topInset + 10, 14, 10),
              child: Row(
                children: [
                  Expanded(
                    child: Text(
                      title,
                      key: const ValueKey('update-title'),
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: style.copyWith(
                        color: failed ? Ember.alarm : Ember.gold,
                      ),
                    ),
                  ),
                  if (action != null) ...[
                    const SizedBox(width: 12),
                    Text(
                      action,
                      key: const ValueKey('update-action'),
                      style: style.copyWith(
                        color: working ? Ember.textDim : Ember.orange,
                      ),
                    ),
                  ],
                ],
              ),
            ),
          ),
          // A thin line that fills as the download goes.
          if (working)
            LinearProgressIndicator(
              key: const ValueKey('update-progress'),
              value: phase == UpdatePhase.downloading && status.progress > 0
                  ? status.progress
                  : null,
              minHeight: 2,
              backgroundColor: Ember.outline,
              color: Ember.orange,
            ),
        ],
      ),
    );
  }
}
