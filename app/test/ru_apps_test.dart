import 'package:awg_client/platform/android/ru_apps.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  List<String> picked(List<InstalledApp> apps) => appsPastTunnel(apps).packages;

  test('Russian package names go past the tunnel', () {
    expect(
      picked(const [
        InstalledApp('ru.sberbankmobile'),
        InstalledApp('ru.oneme.app'),
        InstalledApp('com.yandex.browser'),
        InstalledApp('com.vkontakte.android'),
        InstalledApp('com.vk.vkvideo'),
        InstalledApp('com.avito.android'),
        InstalledApp('com.wildberries.ru'),
        InstalledApp('com.kms.free'),
        InstalledApp('com.drweb'),
        InstalledApp('com.idamob.tinkoff.android'),
      ]),
      hasLength(10),
    );
  });

  test('foreign apps stay inside', () {
    expect(
      picked(const [
        InstalledApp('com.google.android.youtube'),
        InstalledApp('org.telegram.messenger'),
        InstalledApp('com.example.notes', installer: 'com.android.vending'),
        InstalledApp('com.rutracker.app'), // "ru" without the dot is not a sign
        InstalledApp('rubik.cube'),
      ]),
      isEmpty,
    );
  });

  test('anything from RuStore goes past the tunnel', () {
    final r = appsPastTunnel(const [
      InstalledApp('com.example.bank', installer: 'ru.vk.store'),
      InstalledApp('ru.alfabank.mobile.android', installer: 'ru.vk.store'),
      InstalledApp('com.example.game', installer: 'com.android.vending'),
    ]);
    expect(r.packages, ['com.example.bank', 'ru.alfabank.mobile.android']);
    expect(r.byName, 1);
    expect(r.byStore, 1);
  });

  test('blocked foreign apps stay inside even when RuStore installed them', () {
    expect(
      picked(const [
        InstalledApp('org.telegram.messenger', installer: 'ru.vk.store'),
        InstalledApp('com.whatsapp', installer: 'ru.vk.store'),
        InstalledApp('com.google.android.youtube', installer: 'ru.vk.store'),
      ]),
      isEmpty,
    );
  });

  test('the app itself always stays inside', () {
    expect(
      picked(const [InstalledApp(ownPackage, installer: 'ru.vk.store')]),
      isEmpty,
    );
  });

  test('the answer is sorted and without repeats, so two can be compared', () {
    final a = picked(const [
      InstalledApp('ru.b'),
      InstalledApp('ru.a'),
      InstalledApp('ru.b'),
      InstalledApp('com.x', installer: 'ru.vk.store'),
    ]);
    expect(a, ['com.x', 'ru.a', 'ru.b']);
    expect(sameApps(a, ['com.x', 'ru.a', 'ru.b']), isTrue);
    expect(sameApps(a, ['com.x', 'ru.a']), isFalse);
    expect(sameApps(a, ['com.x', 'ru.a', 'ru.c']), isFalse);
  });

  test('the journal line counts without naming', () {
    expect(AppsPastTunnel.none.describe(), 'none');
    final r = appsPastTunnel(const [
      InstalledApp('ru.sberbankmobile'),
      InstalledApp('com.example.bank', installer: 'ru.vk.store'),
    ]);
    expect(r.describe(), '2 (by name 1, from a Russian store 1)');
    expect(r.describe(), isNot(contains('sber')));
  });
}
