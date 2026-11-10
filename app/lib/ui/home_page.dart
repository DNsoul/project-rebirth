import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../core/controller.dart';
import '../core/updater.dart';
import 'error_panel.dart';
import 'stage.dart';
import 'theme.dart';
import 'update_banner.dart';

/// The whole screen, and everything on it.
///
/// In the middle, exactly, is the button, with the light of a fire round it
/// (`stage.dart`): what is happening is in how they look, and nothing is written
/// on them. At the top, only when there is one, is the offer of a newer version.
/// At the bottom, only when something is wrong, is the error, in words.
///
/// The three do not share space. The button is where the middle of the window is
/// and its size follows the window; the other two lie over the top and bottom
/// edges and push nothing, so nothing has to scroll and nothing moves when they
/// come and go.
class HomePage extends StatelessWidget {
  const HomePage({super.key, required this.controller, this.updates});

  final ConnectionView controller;

  /// Where a newer version is offered. Without it there is no such strip.
  final UpdateView? updates;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      backgroundColor: Ember.background,
      body: StreamBuilder<AppStatus>(
        initialData: controller.status,
        stream: controller.statusStream,
        builder: (context, snapshot) {
          final status = snapshot.data!;
          return LayoutBuilder(
            builder: (context, box) {
              // A button that is most of the width of a narrow window and well
              // under half the height of a short one: the strip and the error
              // always have room above and below it.
              final side = math
                  .min(box.maxWidth * 0.6, box.maxHeight * 0.4)
                  .clamp(110.0, 250.0)
                  .toDouble();
              final inset = MediaQuery.paddingOf(context);
              return Stack(
                fit: StackFit.expand,
                children: [
                  Center(
                    child: RebirthStage(
                      phase: status.phase,
                      lostContact: status.lostContact,
                      step: status.step,
                      size: side,
                      onPressed: () => _press(status),
                    ),
                  ),
                  if (updates != null)
                    Positioned(
                      left: 0,
                      right: 0,
                      top: 0,
                      child: UpdateBanner(
                        updates: updates!,
                        topInset: inset.top,
                      ),
                    ),
                  Positioned(
                    left: 0,
                    right: 0,
                    bottom: 0,
                    child: _Problem(
                      status: status,
                      updates: updates,
                      bottomInset: inset.bottom,
                    ),
                  ),
                ],
              );
            },
          );
        },
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
        // The button calls it off: nobody should have to sit through a
        // connection they no longer want.
        unawaited(controller.cancel());
      case Phase.disconnecting:
        break; // already on its way; a second press has nothing to add
    }
  }
}

/// What to say at the bottom: the problem with the connection if there is one,
/// then a failed update, then a warning.
class _Problem extends StatelessWidget {
  const _Problem({
    required this.status,
    required this.updates,
    required this.bottomInset,
  });

  final AppStatus status;
  final UpdateView? updates;
  final double bottomInset;

  Notice? _notice(UpdateStatus? update) {
    if (status.phase == Phase.failed) {
      return Notice(
        status.message.isEmpty
            ? 'Не удалось подключиться. Нажмите кнопку, чтобы повторить.'
            : status.message,
        Severity.error,
      );
    }
    // Waiting for the owner is the one thing said while connecting: the person
    // has to read the digits out to them.
    if (status.phase == Phase.connecting && status.approvalCode.isNotEmpty) {
      return Notice(
        'Ждём одобрения владельца. Код для сверки: ${status.approvalCode}. '
        'Назовите его владельцу; нажатие на кнопку отменит ожидание.',
        Severity.warning,
      );
    }
    if (update != null &&
        update.phase == UpdatePhase.failed &&
        update.message.isNotEmpty) {
      return Notice(update.message, Severity.error);
    }
    if (status.note.isNotEmpty) {
      return Notice(status.note, Severity.warning);
    }
    return null;
  }

  @override
  Widget build(BuildContext context) {
    final u = updates;
    if (u == null) {
      return ErrorPanel(notice: _notice(null), bottomInset: bottomInset);
    }
    return StreamBuilder<UpdateStatus?>(
      initialData: u.status,
      stream: u.statusStream,
      builder: (context, snapshot) =>
          ErrorPanel(notice: _notice(snapshot.data), bottomInset: bottomInset),
    );
  }
}
