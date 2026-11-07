@TestOn('windows')
library;

import 'dart:convert';
import 'dart:io';

import 'package:awg_client/platform/windows/windows_system.dart';
import 'package:awg_client/platform/windows/windows_tunnel.dart';
import 'package:flutter_test/flutter_test.dart';

/// A stand-in for running programs: it records every call and answers from a
/// script, so the driver's commands can be checked without administrator rights
/// and without touching the machine.
class Calls {
  final List<(String, List<String>)> log = [];

  /// Called for each program; return null to answer "succeeded, no output".
  ProcessResult? Function(String exe, List<String> args)? answer;

  Future<ProcessResult> run(String exe, List<String> args) async {
    log.add((exe, args));
    return answer?.call(exe, args) ?? ProcessResult(0, 0, '', '');
  }

  Iterable<List<String>> of(String exeEnding) =>
      log.where((c) => c.$1.endsWith(exeEnding)).map((c) => c.$2);

  bool ran(String exeEnding, String firstArg) =>
      of(exeEnding).any((a) => a.isNotEmpty && a.first == firstArg);
}

ProcessResult ok(String out) => ProcessResult(0, 0, out, '');
ProcessResult fail(int code, [String err = '']) =>
    ProcessResult(0, code, '', err);

void main() {
  late Directory dir;
  late Calls calls;
  late WindowsTunnelDriver driver;
  var serviceRunning = <String>{};

  setUp(() {
    dir = Directory.systemTemp.createTempSync('awgr-test-');
    File('${dir.path}\\amneziawg.exe').writeAsStringSync('');
    File('${dir.path}\\awg.exe').writeAsStringSync('');
    serviceRunning = {};
    calls = Calls()
      ..answer = (exe, args) {
        // The state of one service, as `sc query` prints it. The line reads the
        // same in every language of Windows.
        if (exe == 'sc.exe' && args.first == 'query') {
          final name = args[1];
          return serviceRunning.contains(name)
              ? ok(
                  'SERVICE_NAME: $name \r\n'
                  '        TYPE               : 10  WIN32_OWN_PROCESS  \r\n'
                  '        STATE              : 4  RUNNING \r\n',
                )
              : fail(1060, 'The specified service does not exist.');
        }
        // The list of our services, as `reg query ... /k` prints it.
        if (exe == 'reg.exe' && args.first == 'query' && args.contains('/k')) {
          if (serviceRunning.isEmpty) return fail(1);
          return ok(
            '\r\n${serviceRunning.map((s) => r'HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\' + s).join('\r\n')}'
            '\r\n\r\nEnd of search: ${serviceRunning.length} match(es) found.\r\n',
          );
        }
        if (exe.endsWith('amneziawg.exe') &&
            args.first == '/installtunnelservice') {
          final name = RegExp(
            r'([a-z0-9-]+)\.conf$',
          ).firstMatch(args[1])!.group(1);
          serviceRunning.add('AmneziaWGTunnel\$$name');
        }
        if (exe.endsWith('amneziawg.exe') &&
            args.first == '/uninstalltunnelservice') {
          serviceRunning.remove('AmneziaWGTunnel\$${args[1]}');
        }
        if (exe == 'whoami') {
          return ok(
            'Mandatory Label\\High Mandatory Level  Label  S-1-16-12288\r\n',
          );
        }
        return null;
      };
    driver = WindowsTunnelDriver(
      installDir: dir.path,
      confDir: '${dir.path}\\tunnels',
      run: calls.run,
    );
  });

  tearDown(() => dir.deleteSync(recursive: true));

  group('the tunnel driver', () {
    test(
      'is ready when the client is installed and the app is elevated',
      () async {
        expect(await driver.preflight(), isNull);
      },
    );

    test(
      'says so when the app is not elevated, in any language of Windows',
      () async {
        calls.answer = (exe, args) => exe == 'whoami'
            ? ok(
                'Обязательная метка\\Средний обязательный уровень  Label  S-1-16-8192\r\n',
              )
            : null;
        expect(await driver.preflight(), contains('администратора'));
      },
    );

    test('says so when AmneziaWG is not installed', () async {
      final empty = WindowsTunnelDriver(
        installDir: '${dir.path}\\nowhere',
        confDir: dir.path,
        run: calls.run,
      );
      expect(await empty.preflight(), contains('AmneziaWG'));
    });

    test(
      'installs a tunnel: the file without a byte order mark, the service by hand-start',
      () async {
        await driver.install(
          name: 'awgr-a',
          conf: '[Interface]\nPrivateKey = x\n',
        );

        final bytes = File(
          '${dir.path}\\tunnels\\awgr-a.conf',
        ).readAsBytesSync();
        expect(
          bytes.take(3),
          isNot([0xEF, 0xBB, 0xBF]),
          reason: 'the client rejects a file that starts with a BOM',
        );
        expect(utf8.decode(bytes), '[Interface]\nPrivateKey = x\n');

        final install = calls.of('amneziawg.exe').first;
        expect(install, [
          '/installtunnelservice',
          '${dir.path}\\tunnels\\awgr-a.conf',
        ]);
        expect(
          calls.of('sc.exe').where((a) => a.first == 'config').single,
          [r'config', r'AmneziaWGTunnel$awgr-a', 'start=', 'demand'],
          reason: 'a restart must not bring the tunnel up on its own',
        );
      },
    );

    test(
      'closes the configuration directory to everyone but administrators and the system',
      () async {
        await driver.install(name: 'awgr-a', conf: 'x');
        final acl = calls.of('icacls.exe').single;
        expect(
          acl,
          containsAll([
            '/inheritance:r',
            '*S-1-5-32-544:(OI)(CI)F',
            '*S-1-5-18:(OI)(CI)F',
          ]),
        );

        await driver.install(name: 'awgr-b', conf: 'x');
        expect(calls.of('icacls.exe'), hasLength(1), reason: 'once is enough');
      },
    );

    test('replaces a tunnel of the same name', () async {
      await driver.install(name: 'awgr-a', conf: 'one');
      calls.log.clear();
      await driver.install(name: 'awgr-a', conf: 'two');
      expect(calls.ran('amneziawg.exe', '/uninstalltunnelservice'), isTrue);
      expect(calls.ran('amneziawg.exe', '/installtunnelservice'), isTrue);
      expect(
        File('${dir.path}\\tunnels\\awgr-a.conf').readAsStringSync(),
        'two',
      );
    });

    test(
      'a client that refuses leaves nothing behind and says what it said',
      () async {
        calls.answer = (exe, args) => exe.endsWith('amneziawg.exe')
            ? fail(1, 'Line must occur in a section')
            : null;
        await expectLater(
          driver.install(name: 'awgr-a', conf: 'bad'),
          throwsA(
            isA<TunnelException>().having(
              (e) => e.message,
              'message',
              contains('Line must occur'),
            ),
          ),
        );
        expect(
          File('${dir.path}\\tunnels\\awgr-a.conf').existsSync(),
          isFalse,
          reason: 'a key left on disk for nothing',
        );
      },
    );

    test('removes a tunnel and its file', () async {
      await driver.install(name: 'awgr-a', conf: 'x');
      await driver.uninstall('awgr-a');
      expect(calls.ran('amneziawg.exe', '/uninstalltunnelservice'), isTrue);
      expect(File('${dir.path}\\tunnels\\awgr-a.conf').existsSync(), isFalse);
      expect(await driver.installed(), isEmpty);
    });

    test(
      'removing what is not there is harmless and runs no uninstaller',
      () async {
        await driver.uninstall('awgr-b');
        expect(calls.ran('amneziawg.exe', '/uninstalltunnelservice'), isFalse);
      },
    );

    test('lists only the tunnels it owns', () async {
      serviceRunning = {
        r'AmneziaWGTunnel$awgr-a',
        r'AmneziaWGTunnel$awgr-enroll',
      };
      expect(await driver.installed(), ['awgr-a', 'awgr-enroll']);
      final query = calls.of('reg.exe').single;
      expect(
        query,
        containsAllInOrder(['/f', r'AmneziaWGTunnel$awgr-', '/k']),
        reason:
            'a search for our prefix only, so nobody else\'s tunnel is listed',
      );
    });

    // Starting PowerShell takes most of a second on this machine, a registry or
    // service query a few milliseconds. Taking the tunnel down used to ask
    // PowerShell several times and the program took seven seconds to do it.
    test('never starts PowerShell to look after a tunnel', () async {
      await driver.install(name: 'awgr-a', conf: 'x');
      await driver.installed();
      await driver.latestHandshake('awgr-a');
      await driver.uninstall('awgr-a');
      expect(calls.of('powershell.exe'), isEmpty);
    });

    test('knows by itself when a tunnel is up and when it is gone', () async {
      await driver.install(name: 'awgr-a', conf: 'x');
      expect(
        calls.of('sc.exe').where((a) => a.first == 'query'),
        isNotEmpty,
        reason: 'it looked, rather than waiting a fixed time',
      );
      calls.log.clear();
      await driver.uninstall('awgr-a');
      expect(await driver.installed(), isEmpty);
    });

    test(
      'will not touch a tunnel that is not its own, whatever it is asked',
      () async {
        for (final name in [
          'AmneziaVPN',
          '../evil',
          'awgr-a; calc',
          'awgr-',
          '',
          r'awgr-a$x',
        ]) {
          await expectLater(
            driver.install(name: name, conf: 'x'),
            throwsA(isA<TunnelException>()),
            reason: name,
          );
          await expectLater(
            driver.uninstall(name),
            throwsA(isA<TunnelException>()),
            reason: name,
          );
          await expectLater(
            driver.latestHandshake(name),
            throwsA(isA<TunnelException>()),
            reason: name,
          );
        }
        expect(calls.of('amneziawg.exe'), isEmpty);
      },
    );

    test('reads the latest handshake from the client', () async {
      calls.answer = (exe, args) => exe.endsWith('awg.exe')
          ? ok('AAAA=\t1794040024\r\nBBBB=\t0\r\n')
          : null;
      final hs = await driver.latestHandshake('awgr-a');
      expect(
        hs,
        DateTime.fromMillisecondsSinceEpoch(1794040024 * 1000, isUtc: true),
      );
      expect(calls.of('awg.exe').single, [
        'show',
        'awgr-a',
        'latest-handshakes',
      ]);
    });

    test(
      'no handshake yet, or a tunnel that is not running, is null',
      () async {
        calls.answer = (exe, args) =>
            exe.endsWith('awg.exe') ? ok('AAAA=\t0\r\n') : null;
        expect(await driver.latestHandshake('awgr-a'), isNull);
        calls.answer = (exe, args) => exe.endsWith('awg.exe') ? fail(1) : null;
        expect(await driver.latestHandshake('awgr-a'), isNull);
      },
    );
  });

  group('reading what the system prints', () {
    test('handshakes', () {
      expect(parseLatestHandshake(''), isNull);
      expect(parseLatestHandshake('garbage'), isNull);
      expect(
        parseLatestHandshake('K1=\t100\nK2=\t300\nK3=\t200\n'),
        DateTime.fromMillisecondsSinceEpoch(300000, isUtc: true),
      );
    });

    test('the tunnels found in the registry, in English and Russian output', () {
      const keys =
          r'HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\AmneziaWGTunnel$awgr-a'
          '\r\n'
          r'HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\AmneziaWGTunnel$awgr-enroll'
          '\r\n';
      expect(
        parseTunnelServiceKeys(
          '\r\n${keys}End of search: 2 match(es) found.\r\n',
        ),
        ['awgr-a', 'awgr-enroll'],
      );
      expect(
        parseTunnelServiceKeys(
          '\r\n$keysКонец поиска: найдено совпадений: 2.\r\n',
        ),
        ['awgr-a', 'awgr-enroll'],
      );
      expect(parseTunnelServiceKeys(''), isEmpty);
      expect(parseTunnelServiceKeys('ERROR: unable to find\r\n'), isEmpty);
    });

    test('the state of a service, whatever language it is printed in', () {
      expect(
        parseServiceState('        STATE              : 4  RUNNING \r\n'),
        'running',
      );
      expect(
        parseServiceState('        STATE              : 1  STOPPED \r\n'),
        'stopped',
      );
      expect(
        parseServiceState('        STATE              : 2  START_PENDING \r\n'),
        'state 2',
      );
      expect(parseServiceState(''), isNull);
      expect(parseServiceState('FAILED 1060: no such service'), isNull);
    });

    test('and on a Russian Windows, where the line is not called STATE', () {
      // What sc.exe really printed on the user's machine, as the app reads it:
      // the words are in the console's code page, so the unreadable ones are
      // question marks, and the number with the English state name survives.
      const running =
          '\r\nSERVICE_NAME: AmneziaWGTunnel\$awgr-a\r\n'
          '        ???                : 10  WIN32_OWN_PROCESS\r\n'
          '        ?????????          : 4  RUNNING\r\n'
          '                                (STOPPABLE, NOT_PAUSABLE, ACCEPTS_SHUTDOWN)\r\n'
          '        ???_?????_Win32   : 0  (0x0)\r\n';
      expect(parseServiceState(running), 'running');
      expect(
        parseServiceState(running.replaceFirst('4  RUNNING', '1  STOPPED')),
        'stopped',
      );
      expect(
        parseServiceState(
          running.replaceFirst('4  RUNNING', '2  START_PENDING'),
        ),
        'state 2',
      );
      // The type line carries a number and a word of its own and is not a state.
      expect(
        parseServiceState(
          '        TYPE               : 10  WIN32_OWN_PROCESS\r\n',
        ),
        isNull,
      );
    });

    test('the machine id, in English and in Russian output', () {
      const english =
          '\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Cryptography\r\n'
          '    MachineGuid    REG_SZ    a1b2c3d4-0000-1111-2222-333344445555\r\n\r\n';
      const russian =
          '\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Cryptography\r\n'
          '    MachineGuid    REG_SZ    a1b2c3d4-0000-1111-2222-333344445555\r\n\r\nКонец запроса\r\n';
      expect(parseMachineGuid(english), 'a1b2c3d4-0000-1111-2222-333344445555');
      expect(parseMachineGuid(russian), 'a1b2c3d4-0000-1111-2222-333344445555');
      expect(parseMachineGuid('ERROR: not found'), isNull);
    });

    test('the machine id is read through the registry', () async {
      final c = Calls()
        ..answer = (exe, args) => ok('    MachineGuid    REG_SZ    guid-9\r\n');
      expect(await WindowsMachineId(run: c.run).machineId(), 'guid-9');
      expect(c.log.single.$2, containsAll(['query', 'MachineGuid']));

      final broken = Calls()..answer = (exe, args) => fail(1);
      await expectLater(
        WindowsMachineId(run: broken.run).machineId(),
        throwsA(isA<StateError>()),
      );
    });
  });

  group('name resolution', () {
    test(
      'every name goes to the server, and only our own rule is ever removed',
      () async {
        final c = Calls();
        await WindowsDnsGuard(run: c.run).enable('198.18.0.1');
        final script = c.of('powershell.exe').single.last;

        expect(
          script,
          contains(
            "Add-DnsClientNrptRule -Namespace '.' -NameServers '198.18.0.1'",
          ),
        );
        expect(script, contains("-Comment 'awg-rotator'"));
        expect(
          script,
          contains("Where-Object { \$_.Comment -eq 'awg-rotator' }"),
          reason: 'a rule somebody else made must survive',
        );
        expect(script, contains('Clear-DnsClientCache'));
      },
    );

    test('taking the rule away removes only ours', () async {
      final c = Calls();
      await WindowsDnsGuard(run: c.run).disable();
      final script = c.of('powershell.exe').single.last;
      expect(
        script,
        contains("Where-Object { \$_.Comment -eq 'awg-rotator' }"),
      );
      expect(script, contains('Remove-DnsClientNrptRule -Force'));
      expect(script, isNot(contains('Add-DnsClientNrptRule')));
    });

    test(
      'an address that is not an address is never put into a command',
      () async {
        final c = Calls();
        for (final bad in [
          "1.1.1.1'; calc; '",
          '1.1.1.1 ; shutdown',
          'example.com',
          '',
        ]) {
          await expectLater(
            WindowsDnsGuard(run: c.run).enable(bad),
            throwsA(isA<ArgumentError>()),
            reason: bad,
          );
        }
        expect(c.log, isEmpty);
      },
    );

    test('a failure is reported, not swallowed', () async {
      final c = Calls()..answer = (exe, args) => fail(1, 'Access is denied');
      await expectLater(
        WindowsDnsGuard(run: c.run).enable('198.18.0.1'),
        throwsA(
          isA<StateError>().having(
            (e) => e.message,
            'message',
            contains('Access is denied'),
          ),
        ),
      );
    });
  });

  group('the guard against a restart', () {
    test(
      'installs a start-up task that removes only our resolver rule',
      () async {
        final c = Calls();
        await WindowsBootGuard(run: c.run, dataDir: dir.path).ensureInstalled();

        final script = File('${dir.path}\\cleanup.ps1').readAsStringSync();
        expect(script, contains("\$_.Comment -eq 'awg-rotator'"));
        expect(script, contains('Remove-DnsClientNrptRule'));
        expect(script, isNot(contains('Add-DnsClientNrptRule')));

        final args = c.of('schtasks.exe').single;
        expect(args, containsAllInOrder(['/Create', '/F']));
        expect(args[args.indexOf('/SC') + 1], 'ONSTART');
        expect(args[args.indexOf('/RU') + 1], 'SYSTEM');
        expect(args[args.indexOf('/TR') + 1], contains('cleanup.ps1'));
      },
    );
  });
  group('what programs print', () {
    test('output in a code page that is not UTF-8 does not stop anything', () {
      // "whoami /groups" on a Russian Windows prints Cyrillic in cp866, which is
      // not valid UTF-8. Decoding it strictly threw at start-up and the app
      // never opened.
      final cp866 = [
        0x8E,
        0xA1,
        0xEF,
        0xA7,
        0xA0,
        0xE2,
        0xA5,
        0xAB,
        0xEC,
        0xAD,
        0xA0,
        0xEF,
        0x20,
        0x53,
        0x2D,
        0x31,
        0x2D,
        0x31,
        0x36,
        0x2D,
        0x31,
        0x32,
        0x32,
        0x38,
        0x38,
      ];
      final text = decodeOutput(cp866);
      expect(
        text,
        contains('S-1-16-12288'),
        reason: 'the part that matters is ASCII and survives',
      );
      expect(text, isNot(contains('\uFFFD')));
    });

    test('ordinary UTF-8 is read as it is', () {
      expect(decodeOutput(utf8.encode('Привет ✓')), 'Привет ✓');
    });

    test(
      'a real program whose output is not UTF-8 is read without an exception',
      () async {
        final r = await runProcess('powershell.exe', [
          '-NoProfile',
          '-Command',
          r'$o = [Console]::OpenStandardOutput(); $b = [byte[]](0xE0, 0xE1, 0x41, 0x42); $o.Write($b, 0, 4); $o.Flush()',
        ]);
        expect(r.exitCode, 0);
        expect('${r.stdout}', contains('AB'));
      },
    );

    test('PowerShell is asked to print in UTF-8', () {
      final args = powershellArguments('Get-Date');
      expect(
        args.last,
        startsWith('[Console]::OutputEncoding = [System.Text.Encoding]::UTF8;'),
      );
      expect(args.last, endsWith('Get-Date'));
      expect(args, containsAll(['-NoProfile', '-NonInteractive']));
    });
  });
}
