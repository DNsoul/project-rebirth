// Measures what blocks the isolate that also runs the window.
//
// On Windows Flutter runs Dart and the window's message loop on one thread, so
// whatever stalls the isolate for a second freezes the window for a second: the
// close button, the tray, repainting. A ticker that wants to run every 10 ms
// shows the stalls as gaps between its ticks.
//
//   dart compile exe tool/bench_blocking.dart -o build/bench.exe
//   build/bench.exe

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:awg_proto/awg_proto.dart';

class Ticker {
  final _sw = Stopwatch()..start();
  int _last = 0;
  int worstGapMs = 0;
  late final Timer _timer;

  void start() {
    _last = _sw.elapsedMilliseconds;
    _timer = Timer.periodic(const Duration(milliseconds: 10), (_) {
      final now = _sw.elapsedMilliseconds;
      final gap = now - _last;
      if (gap > worstGapMs) {
        worstGapMs = gap;
      }
      _last = now;
    });
  }

  void stop() => _timer.cancel();
}

Future<void> measure(String what, Future<void> Function() body) async {
  final ticker = Ticker()..start();
  final sw = Stopwatch()..start();
  await body();
  sw.stop();
  // Let a pending tick land, so a stall at the very end is counted.
  await Future<void>.delayed(const Duration(milliseconds: 30));
  ticker.stop();
  stdout.writeln(
    '${what.padRight(58)} took ${sw.elapsedMilliseconds.toString().padLeft(5)} ms,'
    ' longest freeze ${ticker.worstGapMs.toString().padLeft(5)} ms',
  );
}

Future<void> main() async {
  final vectors =
      jsonDecode(File('../spec/vectors.json').readAsStringSync())
          as Map<String, Object?>;
  final keys = vectors['keys']! as Map<String, Object?>;
  final contentKey = decode(keys['content_key']! as String);
  final configDoc =
      (vectors['cases']! as List).cast<Map<String, Object?>>().firstWhere(
        (c) => c['name'] == 'config',
      )['document']!;
  final trust = Trust(
    rootPub: keys['root_pub']! as String,
    backupPub: keys['backup_pub']! as String,
    contentKey: contentKey,
  );
  final blob = await sealJson(
    (configDoc as Map).cast<String, Object?>(),
    contentKey,
  );
  final now = DateTime.fromMillisecondsSinceEpoch(
    (1762400000 + 3600) * 1000,
    isUtc: true,
  );

  // Warm up, so that what is measured is steady-state.
  await trust.openDoc(blob, kindConfig, now, const ProtoState());

  stdout.writeln('--- cryptography, as the app does it');
  await measure('open and verify one signed document (3 times)', () async {
    for (var i = 0; i < 3; i++) {
      await trust.openDoc(blob, kindConfig, now, const ProtoState());
    }
  });
  await measure('make a tunnel key pair', () async {
    await generateTunnelKeyPair();
  });
  await measure('seal and open an envelope (20 times)', () async {
    for (var i = 0; i < 20; i++) {
      await open(contentKey, await seal(contentKey, utf8.encode('x' * 600)));
    }
  });

  stdout.writeln('--- starting programs, as the app does it');
  Future<void> run(String exe, List<String> args) =>
      Process.run(exe, args, stdoutEncoding: null, stderrEncoding: null);
  await measure('whoami /groups', () => run('whoami', ['/groups']));
  await measure(
    'powershell: list services (as installed())',
    () => run('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-Command',
      "Get-Service -Name 'AmneziaWGTunnel\$awgr-*' -ErrorAction SilentlyContinue",
    ]),
  );
  await measure(
    'awg.exe show (as latestHandshake(), 5 times)',
    () async {
      for (var i = 0; i < 5; i++) {
        await run(r'C:\Program Files\AmneziaWG\awg.exe', [
          'show',
          'awgr-a',
          'latest-handshakes',
        ]);
      }
    },
  );
  await measure(
    'reg.exe query MachineGuid',
    () => run('reg.exe', [
      'query',
      r'HKLM\SOFTWARE\Microsoft\Cryptography',
      '/v',
      'MachineGuid',
    ]),
  );
}
