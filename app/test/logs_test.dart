import 'dart:convert';
import 'dart:io';

import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/log_export.dart';
import 'package:awg_client/core/log_journal.dart';
import 'package:awg_client/core/log_scrub.dart';
import 'package:awg_client/core/version.dart';
import 'package:awg_client/platform/android/android_logs.dart';
import 'package:awg_client/platform/windows/downloads_folder.dart';
import 'package:awg_client/ui/home_page.dart';
import 'package:awg_client/ui/theme.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

import 'screen_test.dart' show StaticView, buttonKey, windowOf;

void main() {
  late Directory tmp;
  setUp(() => tmp = Directory.systemTemp.createTempSync('rebirth-logs-'));
  tearDown(() {
    try {
      tmp.deleteSync(recursive: true);
    } on FileSystemException {
      // Windows may hold a file a moment longer.
    }
  });

  group('the scrubber', () {
    final s = LogScrubber(
      secrets: ['0123456789abcdef0123456789abcdef', 'someowner'],
    );

    test('masks the public addresses of servers', () {
      expect(
        s.scrub('a new server is published: sto 203.0.113.45:51820'),
        'a new server is published: sto 203.x.x.45:51820',
      );
      expect(s.scrub('endpoint 8.8.4.4'), 'endpoint 8.x.x.4');
    });

    test('keeps the addresses inside the tunnel and at home', () {
      for (final ip in [
        '10.8.0.5',
        '192.168.1.20',
        '172.20.3.4',
        '127.0.0.1',
        '100.64.1.2',
        '0.0.0.0',
        '198.18.0.1',
      ]) {
        expect(s.scrub('slot works as $ip/32'), 'slot works as $ip/32');
      }
    });

    test('masks public IPv6 addresses and keeps local ones', () {
      expect(
        s.scrub('peer at [2a01:4f8:c0c:1234::1]:51820'),
        'peer at [2a01:…:1]:51820',
      );
      expect(
        s.scrub('2001:0db8:85a3:0000:0000:8a2e:0370:7334 up'),
        '2001:…:7334 up',
      );
      expect(
        s.scrub('fe80::1c2:3ff:fe4a:1 local'),
        'fe80::1c2:3ff:fe4a:1 local',
      );
      expect(s.scrub('fd00::5 tunnel'), 'fd00::5 tunnel');
      expect(s.scrub('loopback ::1'), 'loopback ::1');
    });

    test('leaves clock times and versions alone', () {
      const line = '2026-11-14T09:30:05.123456 connect: settings 412 ms';
      expect(s.scrub(line), line);
      expect(s.scrub('Rebirth 1.4.1 (build 9)'), 'Rebirth 1.4.1 (build 9)');
    });

    test('takes keys out', () {
      const key = 'yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=';
      expect(s.scrub('PrivateKey = $key'), 'PrivateKey = ‹hidden›');
      expect(s.scrub('presharedkey=$key'), 'presharedkey=‹hidden›');
      expect(s.scrub('server $key refused'), 'server ‹key› refused');
    });

    test('takes the gist, tokens and long ids out', () {
      expect(
        s.scrub(
          'fetch https://api.github.com/gists/0123456789abcdef0123456789abcdef failed',
        ),
        'fetch https://api.github.com/… failed',
      );
      expect(
        s.scrub('https://gist.githubusercontent.com/someowner/x/raw/c'),
        'https://gist.githubusercontent.com/…',
      );
      expect(s.scrub('id 0123456789abcdef0123456789abcdef'), 'id ‹secret›');
      expect(s.scrub('owner someowner said'), 'owner ‹secret› said');
      expect(
        s.scrub('token ghp_abcdefghijklmnopqrstuvwxyz0123456789'),
        'token ‹token›',
      );
      expect(s.scrub('etag ${'f' * 40}'), 'etag ‹hex›');
      expect(s.scrub('blob ${'Ab9_' * 12}'), 'blob ‹blob›');
    });

    test('masks an address used as the host of a link', () {
      expect(
        s.scrub('GET http://198.51.100.7:8080/c?x=1'),
        'GET http://198.x.x.7:8080/…',
      );
    });

    test('keeps a stack trace readable', () {
      const frame =
          '#0      ConnectionController._bringUp (package:awg_client/core/controller.dart:1049:9)';
      expect(s.scrub(frame), frame);
    });
  });

  group('the journal', () {
    test('writes each line with the time, scrubbed', () {
      final path = '${tmp.path}/app.log';
      final echoed = <String>[];
      final j = LogJournal(
        path,
        clock: () => DateTime(2026, 11, 14, 9, 30, 5, 123),
        echo: echoed.add,
      );
      j.write('network: changed');
      j.write('a new server is published: sto 203.0.113.45:51820');
      expect(File(path).readAsLinesSync(), [
        '2026-11-14T09:30:05.123 network: changed',
        '2026-11-14T09:30:05.123 a new server is published: sto 203.x.x.45:51820',
      ]);
      expect(echoed.last, contains('203.x.x.45'));
    });

    test('rotates at the limit and keeps one older file', () {
      final path = '${tmp.path}/app.log';
      final j = LogJournal(path, maxBytes: 200);
      for (var i = 0; i < 40; i++) {
        j.write('line $i ${'x' * 20}');
      }
      final now = File(path);
      final old = File('$path.1');
      expect(now.existsSync(), isTrue);
      expect(old.existsSync(), isTrue);
      expect(now.lengthSync(), lessThanOrEqualTo(200));
      expect(old.lengthSync(), lessThanOrEqualTo(200));
      expect(File('$path.2').existsSync(), isFalse);
      // The newest line is in the current file, and the journal reads in order.
      final all = j.readAll().trim().split('\n');
      expect(all.last, endsWith('line 39 ${'x' * 20}'));
      final numbers = [
        for (final l in all)
          int.parse(RegExp(r'line (\d+)').firstMatch(l)![1]!),
      ];
      expect(numbers, orderedEquals([...numbers]..sort()));
      expect(numbers.first, greaterThan(0), reason: 'the oldest are dropped');
    });

    test('picks up the size of a file a previous run left', () {
      final path = '${tmp.path}/app.log';
      File(path).writeAsStringSync('x' * 150);
      LogJournal(path, maxBytes: 200).write('y' * 80);
      expect(File('$path.1').readAsStringSync(), 'x' * 150);
    });

    test('drops a file from before there was a limit', () {
      final path = '${tmp.path}/app.log';
      File(path).writeAsStringSync('unscrubbed 203.0.113.45\n' * 100);
      final j = LogJournal(path, maxBytes: 200)..write('fresh');
      expect(j.readAll(), isNot(contains('203.0.113.45')));
      expect(j.readAll(), contains('fresh'));
    });

    test('a missing journal reads as nothing', () {
      expect(readRotated('${tmp.path}/nothing.log'), '');
    });
  });

  group('putting the journals together', () {
    test('goes by time, keeping stack traces with their line', () {
      const app =
          '2026-11-14T09:30:01.000000 connect: settings\n'
          '2026-11-14T09:30:03.500000 unhandled: boom\n'
          '#0 main (file.dart:1:1)\n'
          '2026-11-14T09:30:09.000000 state: connected\n';
      const native =
          '2026-11-14T09:30:02.000 [android I] VPN service started\n'
          '2026-11-14T09:30:04.000 [awg D] peer - Sending handshake initiation\n';
      expect(mergeJournals([app, native]).split('\n'), [
        '2026-11-14T09:30:01.000000 connect: settings',
        '2026-11-14T09:30:02.000 [android I] VPN service started',
        '2026-11-14T09:30:03.500000 unhandled: boom',
        '#0 main (file.dart:1:1)',
        '2026-11-14T09:30:04.000 [awg D] peer - Sending handshake initiation',
        '2026-11-14T09:30:09.000000 state: connected',
        '',
      ]);
    });

    test('an empty journal changes nothing', () {
      const app = '2026-11-14T09:30:01.000000 a\n';
      expect(mergeJournals([app, '']), app);
    });
  });

  group('saving', () {
    final at = DateTime(2026, 11, 14, 9, 30, 5);

    LogExporter exporter(LogSink sink, {List<String> other = const []}) {
      final j = LogJournal(
        '${tmp.path}/app.log',
        scrubber: LogScrubber(secrets: ['0123456789abcdef0123456789abcdef']),
        clock: () => DateTime(2026, 11, 14, 9, 30, 1),
      )..write('network: changed');
      return LogExporter(
        journal: j,
        otherJournals: other,
        sink: sink,
        device: () async => 'Xiaomi Redmi Note 12, Android 13 (API 33)',
        state: () => AppStatus(Phase.connected, region: 'sto').describe(),
        clock: () => at,
      );
    }

    test('names the file by the moment', () {
      expect(exportFileName(at), 'rebirth-logs-20261114-093005.txt');
    });

    test('writes one file into the folder: header, then both journals', () async {
      final downloads = Directory('${tmp.path}/Downloads');
      final native = '${tmp.path}/native.log';
      File(native).writeAsStringSync(
        '2026-11-14T09:30:00.500 [android I] VPN service started\n'
        '2026-11-14T09:30:02.000 [awg D] endpoint 203.0.113.45:51820 '
        'gist 0123456789abcdef0123456789abcdef\n',
      );
      final name = await exporter(
        DirectoryLogSink(() async => downloads),
        other: [native],
      ).export();
      expect(name, 'rebirth-logs-20261114-093005.txt');
      final lines = File('${downloads.path}/$name').readAsLinesSync();
      expect(lines.take(4), [
        'Rebirth $appVersion (build $appBuild)',
        'device: Xiaomi Redmi Note 12, Android 13 (API 33)',
        startsWith('saved: 2026-11-14T09:30:05.000 UTC'),
        'state: connected sto',
      ]);
      final body = lines
          .skipWhile((l) => !l.startsWith('---'))
          .skip(1)
          .toList();
      expect(body, [
        '2026-11-14T09:30:00.500 [android I] VPN service started',
        '2026-11-14T09:30:01.000 network: changed',
        // The system side's journal is scrubbed again, with this build's secrets.
        '2026-11-14T09:30:02.000 [awg D] endpoint 203.x.x.45:51820 gist ‹secret›',
      ]);
    });

    test('does not overwrite a file of the same name', () async {
      final downloads = Directory('${tmp.path}/Downloads')..createSync();
      File(
        '${downloads.path}/rebirth-logs-20261114-093005.txt',
      ).writeAsStringSync('old');
      final name = await exporter(
        DirectoryLogSink(() async => downloads),
      ).export();
      expect(name, 'rebirth-logs-20261114-093005 (2).txt');
      expect(
        File(
          '${downloads.path}/rebirth-logs-20261114-093005.txt',
        ).readAsStringSync(),
        'old',
      );
    });

    test('says so in words when the folder cannot be written', () async {
      // A file where the folder should be.
      final blocker = File('${tmp.path}/Downloads')..writeAsStringSync('');
      await expectLater(
        exporter(
          DirectoryLogSink(() async => Directory(blocker.path)),
        ).export(),
        throwsA(
          isA<LogExportException>().having(
            (e) => e.message,
            'message',
            exportFailedMessage,
          ),
        ),
      );
    });

    test('notes in the journal what became of it', () async {
      final e = exporter(
        DirectoryLogSink(() async => Directory('${tmp.path}/D')),
      );
      final lines = <String>[];
      await saveJournal(e, lines.add);
      expect(lines, [
        'journal: saving',
        'journal: saved as rebirth-logs-20261114-093005.txt',
      ]);
    });
  });

  test(
    'finds Downloads on Windows',
    () => expect(windowsDownloads().existsSync(), isTrue),
    skip: !Platform.isWindows,
  );

  group('Downloads on Android', () {
    TestWidgetsFlutterBinding.ensureInitialized();
    const channel = MethodChannel('awg/vpn');
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    tearDown(() => messenger.setMockMethodCallHandler(channel, null));

    test('hands the file to the system and answers with its name', () async {
      MethodCall? seen;
      messenger.setMockMethodCallHandler(channel, (call) async {
        seen = call;
        return 'rebirth-logs-20261114-093005 (1).txt';
      });
      final name = await AndroidDownloadsSink().save(
        'rebirth-logs-20261114-093005.txt',
        utf8.encode('hi'),
      );
      expect(name, 'rebirth-logs-20261114-093005 (1).txt');
      expect(seen!.method, 'saveToDownloads');
      expect(seen!.arguments['name'], 'rebirth-logs-20261114-093005.txt');
      expect(utf8.decode(seen!.arguments['bytes'] as List<int>), 'hi');
    });

    test('turns a refusal into words', () async {
      for (final (code, message) in [
        ('denied', exportDeniedMessage),
        ('no_space', exportNoSpaceMessage),
        ('failed', exportFailedMessage),
      ]) {
        messenger.setMockMethodCallHandler(channel, (call) async {
          throw PlatformException(code: code, message: 'x');
        });
        await expectLater(
          AndroidDownloadsSink().save('a.txt', const []),
          throwsA(
            isA<LogExportException>().having((e) => e.message, code, message),
          ),
        );
      }
    });
  });

  group('the state in a line', () {
    test('says what the screen shows', () {
      expect(const AppStatus(Phase.idle).describe(), 'idle');
      expect(
        const AppStatus(
          Phase.connecting,
          message: 'Читаю…',
          step: ConnectStep.tunnel,
        ).describe(),
        'connecting (tunnel)',
      );
      expect(
        AppStatus(
          Phase.connected,
          region: 'sto',
          lostContact: true,
          recovering: true,
        ).describe(),
        'connected sto, lost contact, recovering',
      );
      expect(
        const AppStatus(
          Phase.failed,
          message: 'Сервер не отвечает.',
          step: ConnectStep.server,
        ).describe(),
        'failed (server), Сервер не отвечает.',
      );
    });
  });

  group('the long press', () {
    const window = Size(324, 521);

    Widget screen(StaticView view, Future<String> Function() save) =>
        MaterialApp(
          debugShowCheckedModeBanner: false,
          theme: appTheme(),
          home: HomePage(controller: view, saveLogs: save),
        );

    testWidgets('saves the journal and says where it went', (tester) async {
      windowOf(tester, window);
      final view = StaticView(const AppStatus(Phase.idle));
      var saves = 0;
      await tester.pumpWidget(
        screen(view, () async {
          saves++;
          return 'rebirth-logs-20261114-093005.txt';
        }),
      );
      await tester.longPress(find.byKey(buttonKey));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 300));
      expect(saves, 1);
      expect(view.presses, isEmpty, reason: 'a long press does not connect');
      expect(find.text('Журнал сохранён в Загрузки:'), findsOneWidget);
      expect(find.text('rebirth-logs-20261114-093005.txt'), findsOneWidget);
      // It goes after a while.
      await tester.pump(HomePage.flashFor);
      await tester.pump(const Duration(milliseconds: 300));
      expect(find.textContaining('Журнал сохранён'), findsNothing);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('a short press does what it always did', (tester) async {
      windowOf(tester, window);
      for (final (phase, press) in [
        (Phase.idle, 'connect'),
        (Phase.connected, 'disconnect'),
        (Phase.connecting, 'cancel'),
        (Phase.failed, 'connect'),
      ]) {
        final view = StaticView(AppStatus(phase, region: 'sto'));
        var saves = 0;
        await tester.pumpWidget(
          screen(view, () async {
            saves++;
            return 'x.txt';
          }),
        );
        await tester.tap(find.byKey(buttonKey));
        await tester.pump(const Duration(milliseconds: 600));
        expect(view.presses, [press], reason: '$phase');
        expect(saves, 0, reason: '$phase');
        await tester.pumpWidget(const SizedBox());
      }
    });

    testWidgets('says why when it could not', (tester) async {
      windowOf(tester, window);
      final view = StaticView(AppStatus(Phase.connected, region: 'sto'));
      await tester.pumpWidget(
        screen(view, () async => throw LogExportException(exportDeniedMessage)),
      );
      await tester.longPress(find.byKey(buttonKey));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 300));
      expect(find.text('Не удалось сохранить журнал.'), findsOneWidget);
      expect(
        find.text('Разрешите приложению доступ к файлам и повторите.'),
        findsOneWidget,
      );
      expect(view.presses, isEmpty);
      await tester.pump(HomePage.flashFor);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('an unforeseen failure still gets words', (tester) async {
      windowOf(tester, window);
      final view = StaticView(const AppStatus(Phase.idle));
      await tester.pumpWidget(screen(view, () async => throw StateError('x')));
      await tester.longPress(find.byKey(buttonKey));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 300));
      expect(find.text('Не удалось сохранить журнал.'), findsOneWidget);
      expect(find.text('Повторите ещё раз.'), findsOneWidget);
      await tester.pump(HomePage.flashFor);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('without a way to save, a long press is a press, as before', (
      tester,
    ) async {
      windowOf(tester, window);
      final view = StaticView(const AppStatus(Phase.idle));
      await tester.pumpWidget(
        MaterialApp(
          theme: appTheme(),
          home: HomePage(controller: view),
        ),
      );
      await tester.longPress(find.byKey(buttonKey));
      await tester.pump(const Duration(milliseconds: 300));
      expect(view.presses, ['connect']);
      expect(find.byType(Text), findsNothing);
      await tester.pumpWidget(const SizedBox());
    });
  });
}
