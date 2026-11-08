import 'dart:async';
import 'dart:io';

import 'package:awg_client/core/updater.dart';
import 'package:awg_client/core/version.dart';
import 'package:awg_client/platform/android/apk_installer.dart';
import 'package:awg_client/platform/windows/update_installer.dart';
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

class FakeReleases implements ReleaseSource {
  final _controller = StreamController<Release>.broadcast();
  Release? _release;

  @override
  Release? get release => _release;

  @override
  Stream<Release> get releaseStream => _controller.stream;

  /// Announces [r] and lets the listeners hear it, as the real source's
  /// listeners do a moment after the gist is read.
  Future<void> announce(Release r) async {
    _release = r;
    _controller.add(r);
    await Future<void>.delayed(Duration.zero);
  }
}

class FakeDownloader implements Downloader {
  FakeDownloader(this.bytes);

  List<int> bytes;
  Object? fail;
  Completer<void>? gate;
  int calls = 0;
  Uri? lastUrl;
  int? lastLimit;

  /// The ways it was asked to fetch the file, in order.
  final routesAsked = <DownloadRoute>[];

  /// What a way gives instead of the common [fail] or [bytes]: an error to
  /// throw, or the bytes to write.
  final perRoute = <DownloadRoute, Object>{};

  /// The addresses it was asked for, in order, with the way.
  final asked = <(DownloadRoute, Uri)>[];

  /// What an address gives instead of the common [fail] or [bytes] (the first
  /// match of its text wins).
  final perAddress = <String, Object>{};

  /// The ways it cannot use for an address.
  bool Function(DownloadRoute route, Uri url) usable = (_, _) => true;

  @override
  bool canUse(DownloadRoute route, Uri url) => usable(route, url);

  @override
  Future<void> download(
    Uri url,
    File file, {
    required int limit,
    void Function(int received)? onProgress,
    DownloadRoute route = DownloadRoute.usual,
  }) async {
    calls++;
    lastUrl = url;
    lastLimit = limit;
    routesAsked.add(route);
    asked.add((route, url));
    final g = gate;
    if (g != null) await g.future;
    final byAddress = perAddress.entries
        .where((e) => url.toString().contains(e.key))
        .map((e) => e.value)
        .firstOrNull;
    if (byAddress is Exception) throw byAddress;
    final special = byAddress ?? perRoute[route];
    if (special is Exception) throw special;
    final f = fail;
    if (f != null) throw f;
    final bytes = special is List<int> ? special : this.bytes;
    final sink = file.openWrite();
    var sent = 0;
    for (var i = 0; i < 4; i++) {
      final end = i == 3 ? bytes.length : bytes.length * (i + 1) ~/ 4;
      sink.add(bytes.sublist(sent, end));
      sent = end;
      onProgress?.call(sent);
    }
    await sink.close();
  }
}

class FakeInstaller implements UpdateInstaller {
  final started = <File>[];
  Object? fail;

  @override
  Future<void> install(File file) async {
    final f = fail;
    if (f != null) throw f;
    expect(
      file.existsSync(),
      isTrue,
      reason: 'the installer is given a real file',
    );
    started.add(file);
  }
}

/// Waits, in real time, until [condition] holds: the download folder is made and
/// emptied on the disk before the first byte is asked for.
Future<void> until(bool Function() condition) async {
  for (var i = 0; i < 400 && !condition(); i++) {
    await Future<void>.delayed(const Duration(milliseconds: 5));
  }
  expect(condition(), isTrue, reason: 'never got there');
}

/// An announcement whose installers are the given bytes.
Future<Release> releaseOf(
  List<int> bytes, {
  int build = 12,
  String version = '1.2.0',
  int minBuild = 0,
  bool windows = true,
  bool android = true,
}) async {
  final hash = await sha256Hex(Stream.value(bytes));
  final base = unsignedRelease(
    build: build,
    version: version,
    minBuild: minBuild,
  );
  return Release(
    v: base.v,
    version: base.version,
    build: base.build,
    minBuild: base.minBuild,
    issuedAt: base.issuedAt,
    notes: base.notes,
    assets: [
      if (windows)
        ReleaseAsset(
          os: osWindows,
          url: 'https://example.org/Rebirth-Setup-$version.exe',
          sha256: hash,
          size: bytes.length,
        ),
      if (android)
        ReleaseAsset(
          os: osAndroid,
          url: 'https://example.org/Rebirth-$version.apk',
          sha256: hash,
          size: bytes.length,
        ),
    ],
    issuer: base.issuer,
    sig: base.sig,
  );
}

void main() {
  final installerBytes = Uint8List.fromList(
    List.generate(4000, (i) => i % 251),
  );

  late Directory dir;
  late FakeReleases releases;
  late FakeDownloader downloader;
  late FakeInstaller installer;
  late UpdateController updates;
  late List<String> logs;

  UpdateController make({int build = 1, String os = osWindows}) =>
      UpdateController(
        releases: releases,
        os: os,
        currentBuild: build,
        downloader: downloader,
        installer: installer,
        directory: Directory('${dir.path}${Platform.pathSeparator}updates'),
        log: logs.add,
      );

  setUp(() {
    dir = Directory.systemTemp.createTempSync('rebirth-update-test');
    releases = FakeReleases();
    downloader = FakeDownloader(installerBytes);
    installer = FakeInstaller();
    logs = [];
    updates = make();
  });

  tearDown(() async {
    await updates.dispose();
    dir.deleteSync(recursive: true);
  });

  List<String> filesLeftIn(String folder) {
    final d = Directory('${dir.path}${Platform.pathSeparator}$folder');
    return d.existsSync()
        ? [for (final f in d.listSync()) f.uri.pathSegments.last]
        : <String>[];
  }

  List<String> filesLeft() => filesLeftIn('updates');

  group('whether there is anything to offer', () {
    test('nothing, until something is announced', () {
      expect(updates.status, isNull);
    });

    test('a newer build is offered, with what is new in it', () async {
      await releases.announce(await releaseOf(installerBytes));
      final s = updates.status!;
      expect(s.phase, UpdatePhase.available);
      expect(s.offer.version, '1.2.0');
      expect(s.offer.notes, 'Что нового');
      expect(s.offer.mandatory, isFalse);
    });

    test('the same build, or an older one, is not', () async {
      updates = make(build: 12);
      await releases.announce(await releaseOf(installerBytes, build: 12));
      expect(updates.status, isNull);
      await releases.announce(await releaseOf(installerBytes, build: 11));
      expect(updates.status, isNull);
    });

    test(
      'an announcement made before the program started is seen at once',
      () async {
        await releases.announce(await releaseOf(installerBytes));
        final late = make();
        expect(late.status?.offer.version, '1.2.0');
        await late.dispose();
      },
    );

    test('an installer for another platform is no offer', () async {
      await releases.announce(await releaseOf(installerBytes, windows: false));
      expect(updates.status, isNull);
      final phone = make(os: osAndroid);
      expect(phone.status?.offer.asset.os, osAndroid);
      await phone.dispose();
    });

    test(
      'is mandatory when this build is below the oldest that still works',
      () async {
        await releases.announce(
          await releaseOf(installerBytes, build: 12, minBuild: 5),
        );
        expect(updates.status!.offer.mandatory, isTrue);

        final newer = make(build: 5);
        expect(newer.status!.offer.mandatory, isFalse);
        await newer.dispose();
      },
    );

    test(
      'stays until installed, and a newer announcement replaces it',
      () async {
        await releases.announce(await releaseOf(installerBytes));
        expect(updates.status?.offer.version, '1.2.0');

        await releases.announce(await releaseOf(installerBytes));
        expect(
          updates.status?.offer.version,
          '1.2.0',
          reason: 'the same announcement again',
        );

        await releases.announce(
          await releaseOf(installerBytes, build: 13, version: '1.3.0'),
        );
        expect(updates.status?.offer.version, '1.3.0');
      },
    );
  });

  group('installing', () {
    test('downloads the announced file, checks it, and starts it', () async {
      await releases.announce(await releaseOf(installerBytes));
      final phases = <UpdatePhase>[];
      final progress = <double>[];
      final sub = updates.statusStream.listen((s) {
        if (s == null) return;
        phases.add(s.phase);
        if (s.phase == UpdatePhase.downloading) progress.add(s.progress);
      });

      await updates.install();
      await sub.cancel();

      expect(
        downloader.lastUrl.toString(),
        'https://example.org/Rebirth-Setup-1.2.0.exe',
      );
      expect(
        downloader.lastLimit,
        installerBytes.length,
        reason: 'the download is capped at the announced size',
      );
      expect(installer.started, hasLength(1));
      expect(installer.started.single.readAsBytesSync(), installerBytes);
      expect(installer.started.single.path, endsWith('Rebirth-1.2.0.exe'));
      expect(filesLeft(), ['Rebirth-1.2.0.exe'], reason: 'no .part is left');

      expect(phases.first, UpdatePhase.downloading);
      expect(phases, contains(UpdatePhase.installing));
      expect(progress, isNotEmpty);
      expect(progress, orderedEquals([...progress]..sort()));
      expect(progress.last, closeTo(1, 0.01));
      expect(
        updates.status!.phase,
        UpdatePhase.available,
        reason: 'still here, so the person can try again',
      );
    });

    test('an Android offer is saved as an .apk', () async {
      final phone = make(os: osAndroid);
      await releases.announce(await releaseOf(installerBytes));
      await phone.install();
      expect(installer.started.single.path, endsWith('Rebirth-1.2.0.apk'));
      await phone.dispose();
    });

    test('refuses a file whose contents are not the announced ones', () async {
      await releases.announce(await releaseOf(installerBytes));
      downloader.bytes = Uint8List.fromList(
        List.of(installerBytes)
          ..[1234] ^= 0xFF, // same size, one byte different
      );

      await updates.install();

      expect(installer.started, isEmpty, reason: 'never started');
      final s = updates.status!;
      expect(s.phase, UpdatePhase.failed);
      expect(s.message, contains('не прошёл проверку'));
      expect(filesLeft(), isEmpty, reason: 'the bad file is not kept');
    });

    test('refuses a file of the wrong size', () async {
      await releases.announce(await releaseOf(installerBytes));
      downloader.bytes = installerBytes.sublist(0, 3000);

      await updates.install();

      expect(installer.started, isEmpty);
      expect(updates.status!.phase, UpdatePhase.failed);
      expect(updates.status!.message, contains('размер'));
      expect(filesLeft(), isEmpty);
    });

    test('says what went wrong with the download, and can try again', () async {
      await releases.announce(await releaseOf(installerBytes));
      downloader.fail = UpdateException('Нет связи с сервером.');

      await updates.install();
      expect(updates.status!.phase, UpdatePhase.failed);
      expect(updates.status!.message, 'Нет связи с сервером.');

      downloader.fail = null;
      await updates.install();
      expect(installer.started, hasLength(1));
      expect(updates.status!.phase, UpdatePhase.available);
    });

    group('when one way of fetching the file does not work', () {
      UpdateException trouble(String say) => UpdateException(say);

      test('the usual way is the only one asked, when it works', () async {
        await releases.announce(await releaseOf(installerBytes));
        await updates.install();
        expect(downloader.routesAsked, [DownloadRoute.usual]);
        expect(installer.started, hasLength(1));
      });

      test(
        'the next is tried, and the person never hears of the first',
        () async {
          await releases.announce(await releaseOf(installerBytes));
          downloader.perRoute[DownloadRoute.usual] = trouble(
            'Сервер упал (504).',
          );

          await updates.install();

          expect(downloader.routesAsked, [
            DownloadRoute.usual,
            DownloadRoute.api,
          ]);
          expect(installer.started, hasLength(1));
          expect(updates.status!.phase, UpdatePhase.available);
          expect(
            logs.join('\n'),
            allOf(contains('usual route failed'), contains('api route')),
            reason: 'the log says which way worked',
          );
        },
      );

      test(
        'they are tried in turn: the usual, by the API, round the tunnel',
        () async {
          await releases.announce(await releaseOf(installerBytes));
          downloader.perRoute[DownloadRoute.usual] = trouble('one');
          downloader.perRoute[DownloadRoute.api] = trouble('two');

          await updates.install();

          expect(downloader.routesAsked, [
            DownloadRoute.usual,
            DownloadRoute.api,
            DownloadRoute.direct,
          ]);
          expect(installer.started, hasLength(1));
        },
      );

      test('when none works, the first reason is the one told', () async {
        await releases.announce(await releaseOf(installerBytes));
        downloader.perRoute[DownloadRoute.usual] = trouble('Первая причина.');
        downloader.perRoute[DownloadRoute.api] = trouble('Вторая причина.');
        downloader.perRoute[DownloadRoute.direct] = trouble('Третья причина.');

        await updates.install();

        expect(downloader.routesAsked, hasLength(3));
        expect(installer.started, isEmpty);
        expect(updates.status!.phase, UpdatePhase.failed);
        expect(updates.status!.message, 'Первая причина.');
        expect(filesLeft(), isEmpty);
        expect(
          logs.join('\n'),
          allOf(contains('Вторая причина'), contains('Третья причина')),
          reason: 'the others are in the log',
        );
        expect(
          logs.join('\n'),
          isNot(contains('cleanup')),
          reason:
              'a download that failed before the first byte left no file, and '
              'taking away a file that is not there is not worth a line',
        );

        // And "try again" starts from the usual way again.
        downloader.routesAsked.clear();
        downloader.perRoute.clear();
        await updates.install();
        expect(downloader.routesAsked, [DownloadRoute.usual]);
        expect(installer.started, hasLength(1));
      });

      test(
        'a file that is not the announced one is followed by the next way',
        () async {
          await releases.announce(await releaseOf(installerBytes));
          downloader.perRoute[DownloadRoute.usual] = Uint8List.fromList(
            List.of(installerBytes)..[77] ^= 0xFF,
          );

          await updates.install();

          expect(downloader.routesAsked, [
            DownloadRoute.usual,
            DownloadRoute.api,
          ]);
          expect(installer.started, hasLength(1));
          expect(
            File(installer.started.single.path).readAsBytesSync(),
            installerBytes,
            reason: 'what was installed is the announced file, not the bad one',
          );
        },
      );

      test('a failure that is not about the way is not tried again', () async {
        await releases.announce(await releaseOf(installerBytes));
        downloader.fail = const FileSystemException('диск полон');

        await updates.install();

        expect(downloader.routesAsked, [DownloadRoute.usual]);
        expect(updates.status!.message, contains('диск полон'));
      });

      test('the ways can be chosen', () async {
        final only = UpdateController(
          releases: releases,
          os: osWindows,
          currentBuild: 1,
          downloader: downloader,
          installer: installer,
          directory: Directory('${dir.path}${Platform.pathSeparator}only'),
          routes: const [DownloadRoute.usual],
        );
        await releases.announce(await releaseOf(installerBytes));
        downloader.perRoute[DownloadRoute.usual] = trouble('Нет.');

        await only.install();

        expect(downloader.routesAsked, [DownloadRoute.usual]);
        expect(only.status!.phase, UpdatePhase.failed);
        await only.dispose();
      });
    });

    group('copies of the installer in other places', () {
      const copy = 'https://mirror.example/pkg/{version}/{file}';
      const copyAt = 'https://mirror.example/pkg/1.2.0/Rebirth-Setup-1.2.0.exe';

      UpdateController withCopies(List<String> templates) => UpdateController(
        releases: releases,
        os: osWindows,
        currentBuild: 1,
        downloader: downloader,
        installer: installer,
        directory: Directory('${dir.path}${Platform.pathSeparator}copies'),
        mirrors: templates,
        log: logs.add,
      );

      test('there is only the announced address when there are none', () async {
        await releases.announce(await releaseOf(installerBytes));
        await updates.install();
        expect(downloader.asked.map((a) => a.$2.toString()), [
          'https://example.org/Rebirth-Setup-1.2.0.exe',
        ]);
      });

      test(
        'a copy is tried when the announced address does not work',
        () async {
          final c = withCopies([copy]);
          await releases.announce(await releaseOf(installerBytes));
          downloader.perAddress['example.org'] = UpdateException(
            'Сервер упал.',
          );

          await c.install();

          expect(downloader.asked.map((a) => a.$2.toString()), [
            'https://example.org/Rebirth-Setup-1.2.0.exe',
            copyAt,
          ]);
          expect(installer.started, hasLength(1));
          expect(
            logs.join('\n'),
            allOf(contains('mirror.example'), contains('example.org')),
            reason: 'the log says where it came from',
          );
          await c.dispose();
        },
      );

      test(
        'by way first, address second: a copy comes before the next way',
        () async {
          final c = withCopies([copy]);
          await releases.announce(await releaseOf(installerBytes));
          downloader.fail = UpdateException('Нигде нет.');
          // The way by GitHub's API is only for the announced address.
          downloader.usable = (route, url) =>
              route != DownloadRoute.api || url.host == 'example.org';

          await c.install();

          expect(downloader.asked.map((a) => '${a.$1.name} ${a.$2.host}'), [
            'usual example.org',
            'usual mirror.example',
            'api example.org',
            'direct example.org',
            'direct mirror.example',
          ]);
          expect(c.status!.phase, UpdatePhase.failed);
          expect(
            c.status!.message,
            'Нигде нет.',
            reason: 'the first reason is the one told',
          );
          await c.dispose();
        },
      );

      test('a copy that gives another file is not installed', () async {
        final c = withCopies([copy]);
        await releases.announce(await releaseOf(installerBytes));
        downloader.perAddress['example.org'] = UpdateException('Упал.');
        downloader.perAddress['mirror.example'] = Uint8List.fromList(
          List.of(installerBytes)..[5] ^= 0xFF,
        );

        await c.install();

        expect(installer.started, isEmpty);
        expect(c.status!.phase, UpdatePhase.failed);
        expect(filesLeftIn('copies'), isEmpty);
        await c.dispose();
      });

      test('the file name and version come from the announcement', () async {
        final c = withCopies(['https://a.example/{version}/x/{file}']);
        await releases.announce(
          await releaseOf(installerBytes, version: '2.0.1', build: 20),
        );
        downloader.perAddress['example.org'] = UpdateException('Упал.');

        await c.install();

        expect(
          downloader.asked.last.$2.toString(),
          'https://a.example/2.0.1/x/Rebirth-Setup-2.0.1.exe',
        );
        await c.dispose();
      });

      test(
        'a copy that is not https, or is the announced address, is skipped',
        () async {
          final c = withCopies([
            'http://plain.example/{file}',
            'https://example.org/{file}',
            copy,
            copy,
          ]);
          await releases.announce(await releaseOf(installerBytes));
          downloader.perAddress['example.org'] = UpdateException('Упал.');

          await c.install();

          expect(downloader.asked.map((a) => a.$2.toString()), [
            'https://example.org/Rebirth-Setup-1.2.0.exe',
            copyAt,
          ]);
          await c.dispose();
        },
      );
    });

    test('says what went wrong with starting the installer', () async {
      await releases.announce(await releaseOf(installerBytes));
      installer.fail = UpdateException('Разрешите установку.');

      await updates.install();

      expect(updates.status!.phase, UpdatePhase.failed);
      expect(updates.status!.message, 'Разрешите установку.');
    });

    test('a surprise is reported, not thrown at the screen', () async {
      await releases.announce(await releaseOf(installerBytes));
      downloader.fail = const FileSystemException('диск полон');

      await updates.install();

      expect(updates.status!.phase, UpdatePhase.failed);
      expect(updates.status!.message, contains('диск полон'));
    });

    test('a second press while it is downloading does nothing', () async {
      await releases.announce(await releaseOf(installerBytes));
      downloader.gate = Completer<void>();

      final first = updates.install();
      await until(() => downloader.calls == 1);
      await updates.install();
      expect(downloader.calls, 1);

      downloader.gate!.complete();
      await first;
      expect(installer.started, hasLength(1));
    });

    test(
      'an announcement that arrives meanwhile does not interrupt it',
      () async {
        await releases.announce(await releaseOf(installerBytes));
        downloader.gate = Completer<void>();

        final running = updates.install();
        await until(() => downloader.calls == 1);
        await releases.announce(
          await releaseOf(installerBytes, build: 13, version: '1.3.0'),
        );
        expect(updates.status!.phase, UpdatePhase.downloading);
        expect(updates.status!.offer.version, '1.2.0');

        downloader.gate!.complete();
        await running;
        expect(installer.started.single.path, endsWith('Rebirth-1.2.0.exe'));
      },
    );

    test('clears what an earlier try left behind', () async {
      final folder = Directory('${dir.path}${Platform.pathSeparator}updates')
        ..createSync(recursive: true);
      File(
        '${folder.path}${Platform.pathSeparator}Rebirth-1.0.9.exe',
      ).writeAsStringSync('old');
      File(
        '${folder.path}${Platform.pathSeparator}Rebirth-1.2.0.exe.part',
      ).writeAsStringSync('half');

      await releases.announce(await releaseOf(installerBytes));
      await updates.install();

      expect(filesLeft(), ['Rebirth-1.2.0.exe']);
    });
  });

  group('on Windows', () {
    test('the setup is started silently, then the program leaves', () async {
      final order = <String>[];
      final installer = WindowsUpdateInstaller(
        start: (exe, args) async => order.add('start $exe ${args.join(' ')}'),
        leave: () async => order.add('leave'),
      );
      await installer.install(File(r'C:\x\Rebirth-1.2.0.exe'));
      expect(order, [
        r'start C:\x\Rebirth-1.2.0.exe /SILENT /SUPPRESSMSGBOXES /NORESTART',
        'leave',
      ]);
    });

    test('stays where it is if the setup cannot be started', () async {
      var left = false;
      final installer = WindowsUpdateInstaller(
        start: (exe, args) async => throw const ProcessException('x', []),
        leave: () async => left = true,
      );
      await expectLater(
        installer.install(File('x.exe')),
        throwsA(isA<UpdateException>()),
      );
      expect(
        left,
        isFalse,
        reason: 'quitting without an installer would lose the app',
      );
    });
  });

  group('on Android', () {
    test('the package is shown to the installer', () async {
      final bridge = _FakeApkBridge('started');
      await AndroidUpdateInstaller(bridge).install(File('/data/x.apk'));
      expect(bridge.paths, ['/data/x.apk']);
    });

    test(
      'without the permission, the person is told what to switch on',
      () async {
        final bridge = _FakeApkBridge('needs_permission');
        await expectLater(
          AndroidUpdateInstaller(bridge).install(File('/data/x.apk')),
          throwsA(
            isA<UpdateException>().having(
              (e) => e.message,
              'message',
              contains('Установка неизвестных приложений'),
            ),
          ),
        );
      },
    );

    test('a refusal from the system is reported in words', () async {
      final bridge = _FakeApkBridge(
        '',
        error: PlatformException(code: 'installApk', message: 'no'),
      );
      await expectLater(
        AndroidUpdateInstaller(bridge).install(File('/data/x.apk')),
        throwsA(isA<UpdateException>()),
      );
    });
  });

  test('the build number in the code is the one in pubspec.yaml', () {
    final line = File(
      'pubspec.yaml',
    ).readAsLinesSync().firstWhere((l) => l.startsWith('version:'));
    expect(line.substring('version:'.length).trim(), '$appVersion+$appBuild');
  });
}

class _FakeApkBridge implements ApkBridge {
  _FakeApkBridge(this.answer, {this.error});

  final String answer;
  final Object? error;
  final paths = <String>[];

  @override
  Future<String> installApk(String path) async {
    paths.add(path);
    final e = error;
    if (e != null) throw e;
    return answer;
  }
}
