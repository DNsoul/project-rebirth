import 'dart:async';

import 'package:awg_client/core/updater.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:awg_client/ui/update_banner.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

class _Updates implements UpdateView {
  _Updates([this._status]);

  UpdateStatus? _status;
  final _controller = StreamController<UpdateStatus?>.broadcast();
  int installs = 0;

  void show(UpdateStatus? s) {
    _status = s;
    _controller.add(s);
  }

  @override
  UpdateStatus? get status => _status;

  @override
  Stream<UpdateStatus?> get statusStream => _controller.stream;

  @override
  Future<void> install() async => installs++;
}

UpdateStatus status(
  UpdatePhase phase, {
  bool mandatory = false,
  double progress = 0,
}) {
  final r = unsignedRelease();
  return UpdateStatus(
    phase,
    UpdateOffer(release: r, asset: r.asset(osWindows)!, mandatory: mandatory),
    progress: progress,
  );
}

Widget bar(UpdateView updates, {double topInset = 0}) => MaterialApp(
  theme: appTheme(),
  home: Scaffold(
    backgroundColor: Ember.background,
    body: Align(
      alignment: Alignment.topCenter,
      child: UpdateBanner(updates: updates, topInset: topInset),
    ),
  ),
);

void main() {
  testWidgets('shows nothing when there is nothing to offer', (tester) async {
    await tester.pumpWidget(bar(_Updates()));
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);
    expect(find.byType(Text), findsNothing);
  });

  testWidgets('says which version, and offers it', (tester) async {
    await tester.pumpWidget(bar(_Updates(status(UpdatePhase.available))));
    expect(find.text('Версия 1.2.0'), findsOneWidget);
    expect(find.text('Обновить'), findsOneWidget);
  });

  testWidgets('the whole line is the button', (tester) async {
    final updates = _Updates(status(UpdatePhase.available));
    await tester.pumpWidget(bar(updates));

    // Not the word: the empty middle of the line.
    await tester.tapAt(
      tester.getCenter(find.byKey(const ValueKey('update-install'))),
    );
    expect(updates.installs, 1);
  });

  testWidgets('has no way to put it off', (tester) async {
    await tester.pumpWidget(bar(_Updates(status(UpdatePhase.available))));
    expect(find.byType(IconButton), findsNothing);
    expect(find.byIcon(Icons.close_rounded), findsNothing);
    expect(find.text('Позже'), findsNothing);
  });

  testWidgets('asks to update, when the old version no longer works', (
    tester,
  ) async {
    await tester.pumpWidget(
      bar(_Updates(status(UpdatePhase.available, mandatory: true))),
    );
    expect(find.text('Обновите до 1.2.0'), findsOneWidget);
    expect(find.text('Обновить'), findsOneWidget);
  });

  testWidgets('says how far the download is, and cannot be pressed meanwhile', (
    tester,
  ) async {
    final updates = _Updates(status(UpdatePhase.downloading, progress: 0.42));
    await tester.pumpWidget(bar(updates));
    expect(find.text('Скачиваю 1.2.0'), findsOneWidget);
    expect(find.text('42%'), findsOneWidget);
    final line = tester.widget<LinearProgressIndicator>(
      find.byKey(const ValueKey('update-progress')),
    );
    expect(line.value, closeTo(0.42, 1e-9));
    expect(line.minHeight, lessThanOrEqualTo(3), reason: 'a thin line');

    await tester.tapAt(
      tester.getCenter(find.byKey(const ValueKey('update-install'))),
    );
    expect(updates.installs, 0);
  });

  testWidgets('says the setup is starting', (tester) async {
    await tester.pumpWidget(bar(_Updates(status(UpdatePhase.installing))));
    expect(find.text('Запускаю установщик…'), findsOneWidget);
    expect(find.byKey(const ValueKey('update-action')), findsNothing);
  });

  testWidgets('after a failure turns red and offers to try again', (
    tester,
  ) async {
    final updates = _Updates(status(UpdatePhase.failed));
    await tester.pumpWidget(bar(updates));
    expect(find.text('Сбой обновления'), findsOneWidget);
    final title = tester.widget<Text>(
      find.byKey(const ValueKey('update-title')),
    );
    expect(title.style?.color, Ember.alarm);

    await tester.tap(find.text('Повторить'));
    expect(updates.installs, 1);
    expect(
      find.text('Нет связи'),
      findsNothing,
      reason: 'what went wrong is for the bottom of the window, not here',
    );
  });

  testWidgets('runs under the status bar and keeps its text clear of it', (
    tester,
  ) async {
    await tester.pumpWidget(
      bar(_Updates(status(UpdatePhase.available)), topInset: 30),
    );
    final line = tester.getRect(find.byKey(const ValueKey('update-banner')));
    expect(line.top, 0);
    expect(
      tester.getTopLeft(find.byKey(const ValueKey('update-title'))).dy,
      greaterThanOrEqualTo(30),
    );
  });

  testWidgets('appears when an offer arrives and goes when it is withdrawn', (
    tester,
  ) async {
    final updates = _Updates();
    await tester.pumpWidget(bar(updates));
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);

    updates.show(status(UpdatePhase.available));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('update-banner')), findsOneWidget);

    updates.show(null);
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);
  });

  testWidgets('a long version does not break the line on a phone', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(220, 400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    await tester.pumpWidget(bar(_Updates(status(UpdatePhase.available))));
    expect(tester.takeException(), isNull);
  });
}
