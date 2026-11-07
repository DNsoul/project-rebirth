import 'package:flutter/material.dart';

import 'theme.dart';

enum Severity {
  /// Something did not work.
  error,

  /// Something is not as it should be, but the app is carrying on.
  warning,
}

/// A problem to tell the person about.
@immutable
class Notice {
  const Notice(this.message, this.severity);

  final String message;
  final Severity severity;

  @override
  bool operator ==(Object other) =>
      other is Notice && other.message == message && other.severity == severity;

  @override
  int get hashCode => Object.hash(message, severity);
}

/// Splits a message into what happened and what to do about it: the first
/// sentence, and the rest. A message that is one sentence has no rest.
///
/// The messages are written for this: "Нет связи с GitHub. Проверьте интернет."
(String title, String detail) splitMessage(String message) {
  final text = message.trim();
  final m = RegExp(r'^(.+?[.!?])\s+(.+)$', dotAll: true).firstMatch(text);
  if (m == null) {
    return (text, '');
  }
  return (m.group(1)!, m.group(2)!.trim());
}

/// The one place errors are shown: a flat bar along the bottom edge of the
/// window, in words that say what happened and what to do. Nothing at all when
/// there is nothing wrong.
class ErrorPanel extends StatelessWidget {
  const ErrorPanel({super.key, required this.notice, this.bottomInset = 0});

  final Notice? notice;

  /// The height of whatever the system draws over the bottom of the window. The
  /// bar runs under it and keeps its text clear.
  final double bottomInset;

  @override
  Widget build(BuildContext context) {
    final n = notice;
    return AnimatedSwitcher(
      duration: const Duration(milliseconds: 220),
      transitionBuilder: (child, animation) => FadeTransition(
        opacity: animation,
        child: SizeTransition(
          sizeFactor: animation,
          axisAlignment: 1,
          child: child,
        ),
      ),
      child: n == null
          ? const SizedBox(key: ValueKey('no-error'), width: double.infinity)
          : _Bar(key: ValueKey(n), notice: n, bottomInset: bottomInset),
    );
  }
}

class _Bar extends StatelessWidget {
  const _Bar({super.key, required this.notice, required this.bottomInset});

  final Notice notice;
  final double bottomInset;

  @override
  Widget build(BuildContext context) {
    final error = notice.severity == Severity.error;
    final (title, detail) = splitMessage(notice.message);
    final text = Theme.of(context).textTheme;

    return Container(
      key: const ValueKey('error-panel'),
      width: double.infinity,
      color: error ? const Color(0xFF241210) : const Color(0xFF241B0E),
      padding: EdgeInsets.fromLTRB(14, 10, 14, bottomInset + 10),
      // Selectable, so that an error can be copied and sent to whoever looks after
      // the servers. (Not SelectableText: with a line limit it keeps room for all
      // the lines whether they are there or not.)
      child: SelectionArea(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              title,
              key: const ValueKey('error-title'),
              maxLines: 3,
              overflow: TextOverflow.ellipsis,
              style: text.bodyMedium?.copyWith(
                fontSize: 13,
                fontWeight: FontWeight.w600,
                color: error ? Ember.alarm : Ember.amber,
              ),
            ),
            if (detail.isNotEmpty) ...[
              const SizedBox(height: 2),
              Text(
                detail,
                key: const ValueKey('error-detail'),
                maxLines: 4,
                overflow: TextOverflow.ellipsis,
                style: text.bodySmall?.copyWith(
                  fontSize: 12,
                  color: Ember.textDim,
                ),
              ),
            ],
          ],
        ),
      ),
    );
  }
}
