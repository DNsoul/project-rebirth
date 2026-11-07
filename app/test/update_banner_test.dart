import 'dart:async';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/updater.dart';
import 'package:awg_client/ui/home_page.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

class _View implements ConnectionView {
  @override
  AppStatus get status => const AppStatus(Phase.idle);

  @override
  Stream<AppStatus> get statusStream => const Stream.empty();

  @override
  Future<void> connect() async {}

  @override
  Future<void> disconnect() async {}

  @override
  Future<void> cancel() async {}
}

class _Updates implements UpdateView {
  _Updates([this._status]);

  UpdateStatus? _status;
  final _controller = StreamController<UpdateStatus?>.broadcast();
  int installs = 0;
  int dismissals = 0;

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

  @override
  void dismiss() => dismissals++;
}

UpdateOffer offer({
  bool mandatory = false,
  String notes = 'Быстрее подключается',
}) {
  final r = unsignedRelease(notes: notes);
  return UpdateOffer(
    release: r,
    asset: r.asset(osWindows)!,
    mandatory: mandatory,
  );
}

Widget screen(UpdateView updates) => MaterialApp(
  theme: appTheme(),
  home: HomePage(controller: _View(), hint: 'Подсказка', updates: updates),
);

void main() {
  testWidgets('shows only the usual advice when there is nothing to offer', (
    tester,
  ) async {
    await tester.pumpWidget(screen(_Updates()));
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);
    expect(find.textContaining('Серверы меняются сами'), findsOneWidget);
  });

  testWidgets('offers the new version in the place of the advice', (
    tester,
  ) async {
    await tester.pumpWidget(
      screen(_Updates(UpdateStatus(UpdatePhase.available, offer()))),
    );
    expect(find.text('Доступна версия 1.2.0'), findsOneWidget);
    expect(find.text('Быстрее подключается'), findsOneWidget);
    expect(find.text('Обновить'), findsOneWidget);
    expect(find.text('Позже'), findsOneWidget);
    expect(find.textContaining('Серверы меняются сами'), findsNothing);
  });

  testWidgets('the button installs, and "later" puts it off', (tester) async {
    final updates = _Updates(UpdateStatus(UpdatePhase.available, offer()));
    await tester.pumpWidget(screen(updates));

    await tester.tap(find.byKey(const ValueKey('update-install')));
    await tester.tap(find.byKey(const ValueKey('update-later')));

    expect(updates.installs, 1);
    expect(updates.dismissals, 1);
  });

  testWidgets('a mandatory update leaves no "later"', (tester) async {
    await tester.pumpWidget(
      screen(
        _Updates(UpdateStatus(UpdatePhase.available, offer(mandatory: true))),
      ),
    );
    expect(
      find.textContaining('Нужно обновить приложение до 1.2.0'),
      findsOneWidget,
    );
    expect(find.textContaining('больше не будет работать'), findsOneWidget);
    expect(find.byKey(const ValueKey('update-later')), findsNothing);
    expect(find.byKey(const ValueKey('update-install')), findsOneWidget);
  });

  testWidgets('says how far the download is, and has no buttons meanwhile', (
    tester,
  ) async {
    await tester.pumpWidget(
      screen(
        _Updates(
          UpdateStatus(UpdatePhase.downloading, offer(), progress: 0.42),
        ),
      ),
    );
    expect(find.text('Скачиваю версию 1.2.0… 42%'), findsOneWidget);
    final bar = tester.widget<LinearProgressIndicator>(
      find.byKey(const ValueKey('update-progress')),
    );
    expect(bar.value, closeTo(0.42, 1e-9));
    expect(find.byKey(const ValueKey('update-install')), findsNothing);
  });

  testWidgets('says what went wrong, and offers to try again', (tester) async {
    final updates = _Updates(
      UpdateStatus(
        UpdatePhase.failed,
        offer(),
        message: 'Нет связи с сервером.',
      ),
    );
    await tester.pumpWidget(screen(updates));

    expect(find.text('Обновиться не получилось'), findsOneWidget);
    expect(find.text('Нет связи с сервером.'), findsOneWidget);
    await tester.tap(find.text('Повторить'));
    expect(updates.installs, 1);
  });

  testWidgets('appears when an offer arrives, and goes when it is withdrawn', (
    tester,
  ) async {
    final updates = _Updates();
    await tester.pumpWidget(screen(updates));
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);

    updates.show(UpdateStatus(UpdatePhase.available, offer()));
    await tester.pump();
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('update-banner')), findsOneWidget);

    updates.show(null);
    await tester.pump();
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('update-banner')), findsNothing);
    expect(find.textContaining('Серверы меняются сами'), findsOneWidget);
  });

  testWidgets('the button does not move when the offer appears', (
    tester,
  ) async {
    final updates = _Updates();
    await tester.pumpWidget(screen(updates));
    final before = tester.getCenter(find.byKey(const ValueKey('power-button')));

    updates.show(UpdateStatus(UpdatePhase.available, offer()));
    await tester.pump();
    await tester.pumpAndSettle();

    expect(
      tester.getCenter(find.byKey(const ValueKey('power-button'))),
      before,
    );
  });

  testWidgets('a long note does not break the layout on a phone', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(360 * 3, 640 * 3);
    tester.view.devicePixelRatio = 3;
    addTearDown(tester.view.reset);

    await tester.pumpWidget(
      screen(
        _Updates(
          UpdateStatus(
            UpdatePhase.available,
            offer(notes: 'очень длинное описание ' * 40),
          ),
        ),
      ),
    );
    expect(tester.takeException(), isNull);
    expect(find.text('Обновить'), findsOneWidget);
  });
}
