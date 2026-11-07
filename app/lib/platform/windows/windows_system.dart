import 'dart:convert';
import 'dart:io';

import '../../core/platform.dart';
import 'windows_tunnel.dart' show RunProcess, powershellArguments, runProcess;

/// Name-resolution policy for the machine.
///
/// The split between sites that go through the tunnel and sites that do not is
/// made by the server's resolver, so it has to be the only one asked. Windows
/// otherwise asks every network adapter's resolver at once and uses whichever
/// answers first, which would let the ordinary connection decide. A rule in the
/// name resolution policy table that covers every name (the root, ".") says
/// "ask this one".
///
/// The rule carries a comment that marks it as ours, so removing it never
/// touches anyone else's.
class WindowsDnsGuard implements DnsGuard {
  WindowsDnsGuard({RunProcess? run}) : _run = run ?? runProcess;

  static const marker = 'awg-rotator';

  final RunProcess _run;

  Future<void> _powershell(String script) async {
    final r = await _run('powershell.exe', powershellArguments(script));
    if (r.exitCode != 0) {
      throw StateError(
        'Не удалось изменить правила разрешения имён: ${'${r.stderr}'.trim()}',
      );
    }
  }

  @override
  Future<void> enable(String server) async {
    if (!RegExp(r'^[0-9.]{7,15}$').hasMatch(server)) {
      throw ArgumentError('not an address: $server');
    }
    await _powershell(
      '$_removeOurs; '
      "Add-DnsClientNrptRule -Namespace '.' -NameServers '$server' -Comment '$marker' -DisplayName '$marker'; "
      'Clear-DnsClientCache',
    );
  }

  @override
  Future<void> disable() => _powershell('$_removeOurs; Clear-DnsClientCache');

  static const _removeOurs =
      "Get-DnsClientNrptRule | Where-Object { \$_.Comment -eq '$marker' } | "
      'Remove-DnsClientNrptRule -Force';
}

/// The machine's identifier, which stays the same when the app is reinstalled.
class WindowsMachineId implements MachineIdSource {
  WindowsMachineId({RunProcess? run}) : _run = run ?? runProcess;

  final RunProcess _run;

  @override
  Future<String> machineId() async {
    final r = await _run('reg.exe', [
      'query',
      r'HKLM\SOFTWARE\Microsoft\Cryptography',
      '/v',
      'MachineGuid',
    ]);
    final id = parseMachineGuid('${r.stdout}');
    if (r.exitCode != 0 || id == null) {
      throw StateError('MachineGuid is not readable');
    }
    return id;
  }
}

/// The value from `reg query ... /v MachineGuid`, whose line reads
/// `MachineGuid    REG_SZ    <guid>` in every language of Windows.
String? parseMachineGuid(String output) {
  for (final line in const LineSplitter().convert(output)) {
    final m = RegExp(r'MachineGuid\s+REG_SZ\s+(\S+)').firstMatch(line);
    if (m != null) return m.group(1);
  }
  return null;
}

/// Makes sure that a restart cannot leave the machine without working names.
///
/// The resolver rule outlives the app and the tunnel, and a tunnel is not
/// started again by the system. So if the machine restarts while the VPN is up,
/// the rule would send every lookup to an address nothing answers. This
/// installs a task that runs once at start-up, before anyone signs in, and
/// takes the rule away.
class WindowsBootGuard {
  WindowsBootGuard({RunProcess? run, String? dataDir})
    : _run = run ?? runProcess,
      _dataDir =
          dataDir ??
          '${Platform.environment['ProgramData'] ?? r'C:\ProgramData'}\\awg-rotator';

  static const taskName = 'awg-rotator cleanup';

  final RunProcess _run;
  final String _dataDir;

  Future<void> ensureInstalled() async {
    await Directory(_dataDir).create(recursive: true);
    final script = '$_dataDir\\cleanup.ps1';
    await File(script).writeAsString(
      "Get-DnsClientNrptRule | Where-Object { \$_.Comment -eq '${WindowsDnsGuard.marker}' } | "
      'Remove-DnsClientNrptRule -Force\r\n',
      flush: true,
    );
    // No spaces anywhere in the command, so it needs no quoting.
    final windows = Platform.environment['SystemRoot'] ?? r'C:\Windows';
    final command =
        '$windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe '
        '-NoProfile -ExecutionPolicy Bypass -File $script';
    await _run('schtasks.exe', [
      '/Create',
      '/F',
      '/TN',
      taskName,
      '/SC',
      'ONSTART',
      '/RU',
      'SYSTEM',
      '/RL',
      'HIGHEST',
      '/TR',
      command,
    ]);
  }
}
