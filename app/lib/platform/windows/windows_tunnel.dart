import 'dart:convert';
import 'dart:io';

import '../../core/platform.dart';

export '../../core/platform.dart' show TunnelException;

/// Runs a program and returns what it did. Replaced in tests.
typedef RunProcess =
    Future<ProcessResult> Function(String executable, List<String> arguments);

/// Runs a program the way the app needs it run.
///
/// What a Windows program prints is in the code page of the console, which on a
/// Russian system is not UTF-8, so decoding it as UTF-8 throws on the first
/// Cyrillic letter. That took the app down at start-up on such a system, on the
/// very first command, and is why the output is read as bytes and decoded
/// without being strict. Everything the app looks for in it (a security
/// identifier, a service name, a number) is plain ASCII and comes through
/// intact; only words in a message may be garbled, never the parts that matter.
Future<ProcessResult> runProcess(
  String executable,
  List<String> arguments,
) async {
  final r = await Process.run(
    executable,
    arguments,
    stdoutEncoding: null,
    stderrEncoding: null,
  );
  return ProcessResult(
    r.pid,
    r.exitCode,
    decodeOutput(r.stdout),
    decodeOutput(r.stderr),
  );
}

/// Text from a program's output, whatever code page it was in.
String decodeOutput(Object? raw) => raw is List<int>
    ? utf8.decode(raw, allowMalformed: true).replaceAll('�', '?')
    : '$raw';

/// The arguments that run a PowerShell script, with its output in UTF-8: unlike
/// the programs around it, PowerShell can be told to print in something other
/// than the console's code page, which keeps messages readable.
List<String> powershellArguments(String script) => [
  '-NoProfile',
  '-NonInteractive',
  '-ExecutionPolicy',
  'Bypass',
  '-Command',
  '[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $script',
];

/// Brings tunnels up through the AmneziaWG client that is installed on the
/// machine: its tunnel service does the actual work (the network adapter, the
/// routes, the protocol), and this only hands it a configuration.
///
/// Needs administrator rights, which the app asks for once, when it starts.
class WindowsTunnelDriver implements TunnelDriver {
  WindowsTunnelDriver({
    this.installDir = r'C:\Program Files\AmneziaWG',
    String? confDir,
    RunProcess? run,
    this.log,
  }) : confDir =
           confDir ??
           '${Platform.environment['ProgramData'] ?? r'C:\ProgramData'}\\awg-rotator\\tunnels',
       _run = run ?? runProcess;

  final String installDir;

  /// Where the configurations are written. Only administrators and the system
  /// can read it: a configuration holds the device's private key.
  final String confDir;

  final void Function(String line)? log;
  final RunProcess _run;

  /// The service the AmneziaWG client makes for a tunnel called `x`.
  static const servicePrefix = r'AmneziaWGTunnel$';

  String get _tunnelExe => '$installDir\\amneziawg.exe';
  String get _awgExe => '$installDir\\awg.exe';

  bool _aclDone = false;

  @override
  Future<String?> preflight() async {
    if (!Platform.isWindows) return 'Это приложение работает только в Windows.';
    if (!File(_tunnelExe).existsSync() || !File(_awgExe).existsSync()) {
      return 'Не найден AmneziaWG для Windows. Установите его (github.com/amnezia-vpn/amneziawg-windows-client/releases) и запустите Rebirth снова.';
    }
    if (!await _isElevated()) {
      return 'Нужны права администратора. Закройте Rebirth и запустите его заново: Windows спросит разрешение.';
    }
    return null;
  }

  /// Whether this process runs with the full administrator token, judged by the
  /// integrity level in its own groups, which does not depend on the language of
  /// the system.
  Future<bool> _isElevated() async {
    final r = await _run('whoami', ['/groups']);
    if (r.exitCode != 0) return false;
    final out = '${r.stdout}';
    return out.contains('S-1-16-12288') || out.contains('S-1-16-16384');
  }

  @override
  Future<void> install({required String name, required String conf}) async {
    _checkName(name);
    await _prepareDirectory();

    if (await _serviceState(name) != null) await uninstall(name);

    final path = '$confDir\\$name.conf';
    // Written as bytes with no byte order mark: the client rejects a file that
    // starts with one.
    await File(path).writeAsBytes(utf8.encode(conf), flush: true);

    final r = await _run(_tunnelExe, ['/installtunnelservice', path]);
    if (r.exitCode != 0) {
      await _deleteConf(name);
      throw TunnelException(
        'Не удалось запустить туннель. Повторите попытку. (Код ${r.exitCode}. ${_trim('${r.stderr}${r.stdout}')})',
      );
    }
    // The tunnel is started by hand when the app asks, never by the system: if
    // the machine restarts, nothing sends names to a server that may be gone.
    await _run('sc.exe', ['config', '$servicePrefix$name', 'start=', 'demand']);

    // Generous: a slow start is better waited out than torn down and started
    // again, which is what failing here leads to.
    if (!await _waitFor(
      name,
      running: true,
      within: const Duration(seconds: 45),
    )) {
      throw TunnelException(
        'Туннель не запустился. Повторите попытку; если так повторяется, перезагрузите компьютер.',
      );
    }
    log?.call('tunnel $name is up');
  }

  @override
  Future<void> uninstall(String name) async {
    _checkName(name);
    if (await _serviceState(name) != null) {
      await _run(_tunnelExe, ['/uninstalltunnelservice', name]);
      await _waitFor(name, running: false, within: const Duration(seconds: 15));
    }
    await _deleteConf(name);
  }

  @override
  Future<DateTime?> latestHandshake(String name) async {
    _checkName(name);
    final r = await _run(_awgExe, ['show', name, 'latest-handshakes']);
    if (r.exitCode != 0) return null;
    return parseLatestHandshake('${r.stdout}');
  }

  /// Our tunnels, read from the registry's list of services by key name. That is
  /// a program of a few milliseconds and does not depend on the language of the
  /// system; asking PowerShell the same question took most of a second.
  @override
  Future<List<String>> installed() async {
    final r = await _run('reg.exe', [
      'query',
      r'HKLM\SYSTEM\CurrentControlSet\Services',
      '/f',
      '${servicePrefix}awgr-',
      '/k',
    ]);
    // Nothing found is an error exit with nothing to read.
    if (r.exitCode != 0) return const [];
    return parseTunnelServiceKeys('${r.stdout}');
  }

  // ----------------------------------------------------------------- helpers

  /// Waits until the tunnel's service is running (or gone).
  Future<bool> _waitFor(
    String name, {
    required bool running,
    required Duration within,
  }) async {
    final sw = Stopwatch()..start();
    final deadline = DateTime.now().add(within);
    // Every state the service passes through is written down with the time, so
    // that a slow start can be told from one that never happens.
    String? seen = '\u0000';
    while (true) {
      final state = await _serviceState(name);
      if (state != seen) {
        seen = state;
        log?.call(
          'tunnel $name: service is ${state ?? 'absent'} after ${sw.elapsedMilliseconds} ms',
        );
      }
      if (running ? state == _running : state == null) return true;
      if (!DateTime.now().isBefore(deadline)) {
        log?.call(
          'tunnel $name: gave up after ${sw.elapsedMilliseconds} ms, the service was ${state ?? 'absent'}',
        );
        return false;
      }
      await Future<void>.delayed(const Duration(milliseconds: 150));
    }
  }

  static const _running = 'running';

  /// What state the tunnel's service is in, or null if there is no such service.
  /// Read with `sc query`, whose numbers (4 is running) are the same in every
  /// language of Windows and which answers in a few milliseconds.
  Future<String?> _serviceState(String name) async {
    final r = await _run('sc.exe', ['query', '$servicePrefix$name']);
    if (r.exitCode != 0) return null;
    return parseServiceState('${r.stdout}');
  }

  Future<void> _prepareDirectory() async {
    await Directory(confDir).create(recursive: true);
    if (_aclDone) return;
    // Administrators and the system only: S-1-5-32-544 and S-1-5-18 are those
    // two, by identifier, so it works in every language of Windows.
    await _run('icacls.exe', [
      confDir,
      '/inheritance:r',
      '/grant:r',
      '*S-1-5-32-544:(OI)(CI)F',
      '*S-1-5-18:(OI)(CI)F',
    ]);
    _aclDone = true;
  }

  Future<void> _deleteConf(String name) async {
    try {
      final f = File('$confDir\\$name.conf');
      if (await f.exists()) await f.delete();
    } on FileSystemException {
      // Still in use for a moment; it is overwritten the next time.
    }
  }

  static final _validName = RegExp(r'^awgr-[a-z0-9-]{1,24}$');

  /// A name goes into a file path and a service name, so only ours are accepted.
  void _checkName(String name) {
    if (!_validName.hasMatch(name)) {
      throw TunnelException('Недопустимое имя туннеля: $name');
    }
  }

  static String _trim(String s) =>
      s.trim().length > 200 ? '${s.trim().substring(0, 200)}…' : s.trim();
}

/// The newest handshake in the output of `awg show <name> latest-handshakes`,
/// which is one `public-key<TAB>unix-seconds` line per peer, zero for none yet.
DateTime? parseLatestHandshake(String output) {
  var newest = 0;
  for (final line in const LineSplitter().convert(output)) {
    final parts = line.trim().split(RegExp(r'\s+'));
    if (parts.length < 2) continue;
    final seconds = int.tryParse(parts.last);
    if (seconds != null && seconds > newest) newest = seconds;
  }
  return newest == 0
      ? null
      : DateTime.fromMillisecondsSinceEpoch(newest * 1000, isUtc: true);
}

/// Tunnel names from the output of `reg query ...\Services /f AmneziaWGTunnel$awgr- /k`,
/// which lists one registry key per line, such as
/// `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\AmneziaWGTunnel$awgr-a`,
/// and ends with a line counting them, in the language of the system.
List<String> parseTunnelServiceKeys(String output) {
  const marker = '\\${WindowsTunnelDriver.servicePrefix}';
  return [
    for (final line in const LineSplitter().convert(output))
      if (line.contains(marker))
        line.substring(line.lastIndexOf(marker) + marker.length).trim(),
  ];
}

/// The state in the output of `sc query <service>`: `running` or `stopped`, or
/// `state N` for anything else.
///
/// The name of the line is in the language of the system (on a Russian one it is
/// «Состояние», in a legacy code page that arrives as question marks), but the
/// number and the name of the state after it are not: `4  RUNNING`. So that pair
/// is what is looked for, and nothing around it. Looking for the English word
/// `STATE` worked on a machine that printed English and found nothing on the
/// machine this runs on.
String? parseServiceState(String output) {
  final m = RegExp(
    r'(\d+)\s+(STOPPED|START_PENDING|STOP_PENDING|RUNNING|CONTINUE_PENDING|PAUSE_PENDING|PAUSED)\b',
  ).firstMatch(output);
  if (m == null) {
    return null;
  }
  switch (m.group(1)) {
    case '4':
      return 'running';
    case '1':
      return 'stopped';
    default:
      return 'state ${m.group(1)}';
  }
}
