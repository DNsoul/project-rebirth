@TestOn('windows')
library;

import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/platform/windows/single_instance.dart';
import 'package:awg_client/platform/windows/tray_state.dart';
import 'package:flutter_test/flutter_test.dart';

final since = DateTime.utc(2026, 11, 7, 12);

void main() {
  group('what the icon in the notification area says', () {
    test('disconnected: grey, and offers to connect', () {
      final s = trayStateFor(const AppStatus(Phase.idle));
      expect(s.icon, TrayIcon.idle);
      expect(s.statusLine, 'Отключено');
      expect(s.tooltip, 'Rebirth: отключено');
      expect(s.toggleLabel, 'Подключить');
      expect(s.toggleEnabled, isTrue);
    });

    test('connected: green, names the place, and offers to disconnect', () {
      final s = trayStateFor(
        AppStatus(Phase.connected, region: 'waw', since: since),
      );
      expect(s.icon, TrayIcon.connected);
      expect(s.statusLine, 'Подключено · Варшава');
      expect(s.tooltip, 'Rebirth: подключено · Варшава');
      expect(s.toggleLabel, 'Отключить');
      expect(s.toggleEnabled, isTrue);
    });

    test('a server that has gone quiet is not shown as connected', () {
      final s = trayStateFor(
        AppStatus(
          Phase.connected,
          region: 'sto',
          since: since,
          lostContact: true,
        ),
      );
      expect(s.icon, TrayIcon.busy, reason: 'green would be a lie');
      expect(s.statusLine, contains('Нет ответа'));
      expect(s.statusLine, contains('Стокгольм'));
      expect(s.toggleLabel, 'Отключить');
    });

    test('work in progress: amber, and it can be called off', () {
      for (final phase in [Phase.connecting, Phase.switching]) {
        final s = trayStateFor(AppStatus(phase, region: 'ams'));
        expect(s.icon, TrayIcon.busy, reason: '$phase');
        expect(s.toggleEnabled, isTrue, reason: '$phase');
        expect(s.toggleLabel, startsWith('Отменить'), reason: '$phase');
      }
      expect(
        trayStateFor(const AppStatus(Phase.switching)).statusLine,
        'Меняю сервер…',
      );
    });

    test('taking the tunnel down is amber and has nothing to press', () {
      final s = trayStateFor(const AppStatus(Phase.disconnecting));
      expect(s.icon, TrayIcon.busy);
      expect(s.toggleEnabled, isFalse);
    });

    test('a failure: red, says why on hover, and offers to try again', () {
      final s = trayStateFor(
        const AppStatus(Phase.failed, message: 'Сервер не отвечает.'),
      );
      expect(s.icon, TrayIcon.error);
      expect(s.statusLine, 'Не получилось');
      expect(s.tooltip, 'Rebirth: Сервер не отвечает.');
      expect(s.toggleLabel, 'Повторить');
      expect(s.toggleEnabled, isTrue);
    });

    test('a tooltip that is too long for Windows is cut, not refused', () {
      final long = 'очень длинное сообщение ' * 20;
      final s = trayStateFor(AppStatus(Phase.failed, message: long));
      expect(s.tooltip.length, lessThan(maxTooltip));
      expect(s.tooltip, endsWith('…'));
      expect(s.tooltip, startsWith('Rebirth: очень'));
    });

    test('a place the app has not heard of is shown as it is', () {
      expect(
        trayStateFor(AppStatus(Phase.connected, region: 'xyz')).statusLine,
        'Подключено · xyz',
      );
    });

    test('the same status always gives the same state, so nothing redraws', () {
      expect(
        trayStateFor(AppStatus(Phase.connected, region: 'waw', since: since)),
        trayStateFor(
          AppStatus(
            Phase.connected,
            region: 'waw',
            since: since.add(const Duration(hours: 3)),
          ),
        ),
        reason: 'uptime is not shown, so ticking it must not touch the icon',
      );
    });

    test('every phase has an answer, and each icon is used', () {
      final used = {
        for (final p in Phase.values) trayStateFor(AppStatus(p)).icon,
      };
      expect(used, TrayIcon.values.toSet());
    });
  });

  group('the icon files', () {
    // An icon file: a header, a directory of images, the images. Reading it the
    // way Windows does shows that every size is there and well formed.
    List<({int size, int bits, bool png, int length})> read(String path) {
      final b = File(path).readAsBytesSync();
      final d = ByteData.sublistView(b);
      expect(d.getUint16(0, Endian.little), 0, reason: '$path: reserved');
      expect(d.getUint16(2, Endian.little), 1, reason: '$path: type is icon');
      final count = d.getUint16(4, Endian.little);
      final entries = <({int size, int bits, bool png, int length})>[];
      for (var i = 0; i < count; i++) {
        final o = 6 + 16 * i;
        final w = b[o] == 0 ? 256 : b[o];
        final h = b[o + 1] == 0 ? 256 : b[o + 1];
        expect(h, w, reason: '$path: square');
        final length = d.getUint32(o + 8, Endian.little);
        final offset = d.getUint32(o + 12, Endian.little);
        expect(offset + length, lessThanOrEqualTo(b.length), reason: path);

        final png =
            b[offset] == 0x89 &&
            b[offset + 1] == 0x50 &&
            b[offset + 2] == 0x4E &&
            b[offset + 3] == 0x47;
        if (!png) {
          // A bitmap: its own header says the same size, twice as tall (the
          // second half is the mask), 32 bits a pixel.
          expect(
            d.getUint32(offset, Endian.little),
            40,
            reason: '$path: header size',
          );
          expect(d.getInt32(offset + 4, Endian.little), w, reason: path);
          expect(d.getInt32(offset + 8, Endian.little), h * 2, reason: path);
          expect(d.getUint16(offset + 14, Endian.little), 32, reason: path);
        }
        entries.add((
          size: w,
          bits: d.getUint16(o + 6, Endian.little),
          png: png,
          length: length,
        ));
      }
      return entries;
    }

    // The colour of the middle of the 32-pixel image, which is where the symbol
    // is, so look a little off centre at the disc itself.
    String discColour(String path) {
      final b = File(path).readAsBytesSync();
      final d = ByteData.sublistView(b);
      final count = d.getUint16(4, Endian.little);
      for (var i = 0; i < count; i++) {
        final o = 6 + 16 * i;
        if (b[o] != 32) continue;
        final offset = d.getUint32(o + 12, Endian.little);
        // Pixels start after the 40-byte header, bottom row first. A point in the
        // disc, off the symbol: column 4, row 16 from the top.
        final row = 32 - 1 - 16;
        final p = offset + 40 + (row * 32 + 4) * 4;
        return '${b[p + 2]},${b[p + 1]},${b[p]},${b[p + 3]}'; // r,g,b,a
      }
      fail('$path has no 32-pixel image');
    }

    final tray = Directory('assets/tray');

    test('every state has a file, and every file is a state', () {
      final names = tray
          .listSync()
          .map((e) => e.uri.pathSegments.last)
          .where((n) => n.endsWith('.ico'))
          .toSet();
      expect(names, {for (final i in TrayIcon.values) i.asset.split('/').last});
    });

    test('are well-formed, with the sizes the notification area asks for', () {
      for (final icon in TrayIcon.values) {
        final entries = read(icon.asset);
        expect(
          entries.map((e) => e.size),
          containsAll([16, 24, 32]),
          reason: icon.asset,
        );
        expect(
          entries.every((e) => e.bits == 32),
          isTrue,
          reason: 'with transparency',
        );
        expect(
          entries.every((e) => !e.png),
          isTrue,
          reason: 'plain bitmaps, which any loader reads',
        );
      }
    });

    test('are visibly different from one another', () {
      final colours = {
        for (final icon in TrayIcon.values) icon: discColour(icon.asset),
      };
      expect(
        colours.values.toSet(),
        hasLength(TrayIcon.values.length),
        reason: '$colours',
      );
      for (final c in colours.values) {
        expect(c.split(',').last, '255', reason: 'the disc is opaque: $c');
      }
    });

    test('the program has an icon of its own, with a large size too', () {
      final entries = read('windows/runner/resources/app_icon.ico');
      expect(entries.map((e) => e.size), containsAll([16, 32, 48, 256]));
    });
  });

  group('one copy at a time', () {
    late Directory dir;
    setUp(() => dir = Directory.systemTemp.createTempSync('awgr-lock-'));
    tearDown(() => dir.deleteSync(recursive: true));

    test(
      'the second copy cannot take the lock while the first holds it',
      () async {
        final path = '${dir.path}\\sub\\app.lock';
        final first = await SingleInstance.acquire(path);
        expect(first, isNotNull);
        expect(
          await SingleInstance.acquire(path),
          isNull,
          reason: 'it would remove the first one\'s tunnels on start',
        );
        expect(
          await SingleInstance.acquire(path),
          isNull,
          reason: 'and again, nothing is left half-taken by the refusals',
        );

        await first!.release();
        final next = await SingleInstance.acquire(path);
        expect(
          next,
          isNotNull,
          reason: 'once the first has gone, the next may start',
        );
        await next!.release();
      },
    );

    test('different places do not get in each other\'s way', () async {
      final a = await SingleInstance.acquire('${dir.path}\\a.lock');
      final b = await SingleInstance.acquire('${dir.path}\\b.lock');
      expect(a, isNotNull);
      expect(b, isNotNull);
      await a!.release();
      await b!.release();
    });

    test('a lock file left behind by a crash does not stop anything', () async {
      final path = '${dir.path}\\app.lock';
      File(path).writeAsStringSync('left over');
      final instance = await SingleInstance.acquire(path);
      expect(
        instance,
        isNotNull,
        reason: 'the lock is held by the process, not by the file existing',
      );
      await instance!.release();
    });

    test('with no window of ours open there is nothing to bring forward', () {
      // A class no program has, so that a copy of the app that happens to be
      // running on the machine running the tests is neither found nor raised.
      expect(
        bringRunningWindowToFront(windowClass: 'AWG_TEST_NO_SUCH_WINDOW'),
        isFalse,
      );
    });
  });
}
