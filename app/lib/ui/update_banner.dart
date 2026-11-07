import 'dart:async';

import 'package:flutter/material.dart';

import '../core/updater.dart';
import 'theme.dart';

/// Says that a newer version exists and offers to install it. Shows nothing
/// when there is nothing to offer.
class UpdateBanner extends StatelessWidget {
  const UpdateBanner({
    super.key,
    required this.updates,
    required this.fallback,
  });

  final UpdateView updates;

  /// What stands in this place when there is no offer.
  final Widget fallback;

  @override
  Widget build(BuildContext context) {
    return StreamBuilder<UpdateStatus?>(
      initialData: updates.status,
      stream: updates.statusStream,
      builder: (context, snapshot) {
        final s = snapshot.data;
        return AnimatedSwitcher(
          duration: const Duration(milliseconds: 250),
          child: s == null
              ? KeyedSubtree(key: const ValueKey('no-update'), child: fallback)
              : _Card(
                  key: const ValueKey('update-banner'),
                  status: s,
                  onInstall: () => unawaited(updates.install()),
                  onLater: updates.dismiss,
                ),
        );
      },
    );
  }
}

class _Card extends StatelessWidget {
  const _Card({
    super.key,
    required this.status,
    required this.onInstall,
    required this.onLater,
  });

  final UpdateStatus status;
  final VoidCallback onInstall;
  final VoidCallback onLater;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context).textTheme;
    final offer = status.offer;
    final phase = status.phase;

    final String title;
    String? line;
    switch (phase) {
      case UpdatePhase.available:
        title = offer.mandatory
            ? 'Нужно обновить приложение до ${offer.version}'
            : 'Доступна версия ${offer.version}';
        line = offer.mandatory
            ? 'Эта версия больше не будет работать с серверами.'
            : (offer.notes.isEmpty ? null : offer.notes);
      case UpdatePhase.downloading:
        final pct = (status.progress * 100).floor();
        title = pct > 0
            ? 'Скачиваю версию ${offer.version}… $pct%'
            : 'Скачиваю версию ${offer.version}…';
      case UpdatePhase.installing:
        title = 'Запускаю установщик…';
        line = 'Приложение закроется и откроется заново.';
      case UpdatePhase.failed:
        title = 'Обновиться не получилось';
        line = status.message;
    }

    final failed = phase == UpdatePhase.failed;
    final working =
        phase == UpdatePhase.downloading || phase == UpdatePhase.installing;

    return Container(
      width: double.infinity,
      padding: const EdgeInsets.fromLTRB(16, 14, 16, 12),
      decoration: BoxDecoration(
        color: Ember.surfaceHigh,
        borderRadius: BorderRadius.circular(16),
        border: Border.all(color: failed ? Ember.alarm : Ember.outline),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            title,
            key: const ValueKey('update-title'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: theme.titleSmall?.copyWith(
              color: failed ? Ember.alarm : Ember.gold,
              fontWeight: FontWeight.w600,
            ),
          ),
          if (line != null) ...[
            const SizedBox(height: 4),
            Text(
              line,
              key: const ValueKey('update-line'),
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: theme.bodySmall?.copyWith(color: Ember.textDim),
            ),
          ],
          const SizedBox(height: 10),
          if (working)
            ClipRRect(
              borderRadius: BorderRadius.circular(4),
              child: LinearProgressIndicator(
                key: const ValueKey('update-progress'),
                value: phase == UpdatePhase.downloading && status.progress > 0
                    ? status.progress
                    : null,
                minHeight: 6,
                backgroundColor: Ember.outline,
              ),
            )
          else
            Row(
              children: [
                FilledButton(
                  key: const ValueKey('update-install'),
                  onPressed: onInstall,
                  style: FilledButton.styleFrom(
                    backgroundColor: Ember.orange,
                    foregroundColor: const Color(0xFF2A1205),
                    side: BorderSide.none,
                    padding: const EdgeInsets.symmetric(
                      horizontal: 20,
                      vertical: 8,
                    ),
                    // The banner is small; the button is only as tall as it looks.
                    minimumSize: const Size(0, 36),
                    tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                  ),
                  child: Text(failed ? 'Повторить' : 'Обновить'),
                ),
                if (!offer.mandatory) ...[
                  const SizedBox(width: 8),
                  TextButton(
                    key: const ValueKey('update-later'),
                    onPressed: onLater,
                    style: TextButton.styleFrom(
                      foregroundColor: Ember.textDim,
                      minimumSize: const Size(0, 36),
                      tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                    ),
                    child: const Text('Позже'),
                  ),
                ],
              ],
            ),
        ],
      ),
    );
  }
}
