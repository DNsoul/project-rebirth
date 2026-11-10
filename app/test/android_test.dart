import 'dart:convert';
import 'dart:io';

import 'package:awg_client/core/platform.dart';
import 'package:awg_client/platform/android/android_system.dart';
import 'package:awg_client/platform/android/android_tunnel.dart';
import 'package:awg_client/platform/android/awg_native.dart';
import 'package:awg_client/platform/android/conf_plan.dart';
import 'package:awg_client/platform/android/vpn_bridge.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

/// The tunnel library, played by the test.
class FakeNative implements AwgNative {
  int nextHandle = 1;

  /// When set, [turnOn] fails with this code.
  int? failWith;
  String error = 'boom';

  /// What each handle reports.
  final handshakes = <int, int>{};
  final sockets = <int, List<int>>{};

  final calls = <String>[];
  final confs = <String>[];
  final fds = <int>[];
  Object? versionError;

  @override
  String version() {
    final e = versionError;
    if (e != null) throw e;
    return 'awglib test';
  }

  @override
  int turnOn(String conf, int tunFd) {
    calls.add('turnOn');
    confs.add(conf);
    fds.add(tunFd);
    if (failWith != null) return failWith!;
    final h = nextHandle++;
    sockets[h] = [100 + h, -1];
    return h;
  }

  @override
  void turnOff(int handle) => calls.add('turnOff $handle');

  /// How many tunnels the library finds running that nobody owns.
  int strays = 0;

  @override
  int turnOffAll() {
    calls.add('turnOffAll');
    final n = strays;
    strays = 0;
    return n;
  }

  /// What [rebind] answers, and what it did to each handle's sockets.
  int rebindResult = 0;
  final rebound = <int>[];

  @override
  int rebind(int handle) {
    calls.add('rebind $handle');
    rebound.add(handle);
    if (rebindResult == 0) {
      // New sockets, new descriptors.
      sockets[handle] = [200 + handle + rebound.length, -1];
    }
    return rebindResult;
  }

  @override
  int latestHandshake(int handle) => handshakes[handle] ?? 0;

  @override
  int socketV4(int handle) => sockets[handle]?[0] ?? -1;

  @override
  int socketV6(int handle) => sockets[handle]?[1] ?? -1;

  @override
  String lastError() => error;
}

/// The Android side, played by the test.
class FakeBridge implements VpnBridge {
  bool permission = true;
  Object? prepareError;
  Object? establishError;
  int nextFd = 40;
  String id = 'abc123';

  final calls = <String>[];
  final established = <Map<String, Object?>>[];
  final protectedFds = <int>[];
  bool protectWorks = true;

  @override
  void Function()? onRevoked;

  @override
  void Function(bool available)? onNetworkChanged;

  @override
  Future<bool> prepare() async {
    calls.add('prepare');
    final e = prepareError;
    if (e != null) throw e;
    return permission;
  }

  @override
  Future<int> establish({
    required String session,
    required List<String> addresses,
    required List<String> routes,
    required List<String> dns,
    required int mtu,
  }) async {
    calls.add('establish');
    final e = establishError;
    if (e != null) throw e;
    established.add({
      'session': session,
      'addresses': addresses,
      'routes': routes,
      'dns': dns,
      'mtu': mtu,
    });
    return nextFd++;
  }

  @override
  Future<bool> protect(int fd) async {
    calls.add('protect $fd');
    protectedFds.add(fd);
    return protectWorks;
  }

  @override
  Future<void> release() async => calls.add('release');

  final statuses = <String>[];

  @override
  Future<void> setStatus(String text) async => statuses.add(text);

  /// What the person answers to each of the questions the app asks at the start,
  /// and what goes wrong, if anything, by the name of the question.
  final answers = {
    'notifications': 'granted',
    'battery': 'granted',
    'install': 'granted',
  };
  final failures = <String, Object>{};

  Future<String> _asked(String question) async {
    calls.add(question);
    final e = failures[question];
    if (e != null) throw e;
    return answers[question]!;
  }

  @override
  Future<String> ensureNotifications() => _asked('notifications');

  @override
  Future<String> ensureBatteryExemption() => _asked('battery');

  @override
  Future<String> ensureInstallPermission() => _asked('install');

  @override
  Future<String> androidId() async => id;

  @override
  Future<String> filesDir() async => '/data/test';
}

const mainConf =
    '[Interface]\n'
    'PrivateKey = ISEhISEhISEhISEhISEhISEhISEhISEhISEhISEhISE=\n'
    'Address = 10.8.0.7/32\n'
    'DNS = 198.18.0.1\n'
    'MTU = 1280\n'
    'Jc = 4\n'
    '\n'
    '[Peer]\n'
    'PublicKey = ERERERERERERERERERERERERERERERERERERERERERE=\n'
    'AllowedIPs = 198.18.0.0/15, 91.108.4.0/22\n'
    'Endpoint = 203.0.113.9:41820\n'
    'PersistentKeepalive = 25\n';

const enrollConf =
    '[Interface]\n'
    'PrivateKey = ExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExM=\n'
    'Address = 10.8.0.251/32\n'
    'MTU = 1280\n'
    '\n'
    '[Peer]\n'
    'PublicKey = ERERERERERERERERERERERERERERERERERERERERERE=\n'
    'AllowedIPs = 10.8.0.1/32\n'
    'Endpoint = 203.0.113.9:41820\n';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  group('what the system is told about a tunnel', () {
    test('addresses, networks, name servers and size, from the file', () {
      final p = planFromConf(mainConf);
      expect(p.addresses, ['10.8.0.7/32']);
      expect(p.routes, ['198.18.0.0/15', '91.108.4.0/22']);
      expect(p.dns, ['198.18.0.1']);
      expect(p.mtu, 1280);
    });

    test('the enrollment tunnel reaches only the registration address', () {
      final p = planFromConf(enrollConf);
      expect(p.routes, ['10.8.0.1/32']);
      expect(p.dns, isEmpty);
    });

    test('every file the apps are specified to write can be read', () {
      final raw = File('../spec/conf-vectors.json').readAsStringSync();
      final cases = (jsonDecode(raw) as Map)['cases'] as List;
      expect(cases, isNotEmpty);
      for (final c in cases) {
        final conf = (c as Map)['conf'] as String;
        final p = planFromConf(conf);
        expect(p.addresses, [c['address']], reason: '${c['name']}');
        expect(p.routes, isNotEmpty, reason: '${c['name']}');
        expect(p.mtu, 1280, reason: '${c['name']}');
      }
    });

    test('bare addresses become hosts, and the line ends do not matter', () {
      final p = planFromConf(
        '[Interface]\r\nAddress = 10.8.0.7, fd00::7\r\nDNS = 1.1.1.1, 9.9.9.9\r\n'
        '[Peer]\r\nAllowedIPs = 0.0.0.0/0, ::/0\r\n',
      );
      expect(p.addresses, ['10.8.0.7/32', 'fd00::7/128']);
      expect(p.routes, ['0.0.0.0/0', '::/0']);
      expect(p.dns, ['1.1.1.1', '9.9.9.9']);
      expect(p.mtu, 1280, reason: 'the default when the file gives none');
    });

    test('a file without an address or without networks is refused', () {
      expect(
        () => planFromConf(
          '[Interface]\nMTU = 1280\n[Peer]\nAllowedIPs = 10.8.0.1/32\n',
        ),
        throwsA(isA<TunnelException>()),
      );
      expect(
        () => planFromConf(
          '[Interface]\nAddress = 10.8.0.7/32\n[Peer]\nEndpoint = 1.2.3.4:5\n',
        ),
        throwsA(isA<TunnelException>()),
      );
    });
  });

  group('the Android tunnel driver', () {
    late FakeNative native;
    late FakeBridge bridge;
    late AndroidTunnelDriver driver;
    late List<String> log;

    setUp(() {
      native = FakeNative();
      bridge = FakeBridge();
      log = [];
      driver = AndroidTunnelDriver(
        native: native,
        bridge: bridge,
        log: log.add,
      );
    });

    test(
      'is ready when the library loads and the permission is given',
      () async {
        expect(await driver.preflight(), isNull);
        expect(bridge.calls, ['prepare']);
      },
    );

    test('says so when the user refuses the VPN permission', () async {
      bridge.permission = false;
      expect(await driver.preflight(), contains('VPN'));
    });

    test(
      'says so when the library does not load, and does not even ask',
      () async {
        native.versionError = ArgumentError('no such library');
        expect(await driver.preflight(), contains('библиотека'));
        expect(
          bridge.calls,
          isEmpty,
          reason: 'there is no point asking for a permission it cannot use',
        );
      },
    );

    test('says so when the permission cannot be asked for', () async {
      bridge.prepareError = PlatformException(code: 'busy');
      expect(await driver.preflight(), isNotNull);
    });

    test(
      'opens the interface from the file, then starts the library on it',
      () async {
        await driver.install(name: 'awgr-a', conf: mainConf);

        expect(bridge.established.single, {
          'session': 'Rebirth',
          'addresses': ['10.8.0.7/32'],
          'routes': ['198.18.0.0/15', '91.108.4.0/22'],
          'dns': ['198.18.0.1'],
          'mtu': 1280,
        });
        expect(native.fds, [
          40,
        ], reason: 'the descriptor the system gave is the one used');
        expect(
          native.confs.single,
          mainConf,
          reason: 'the library reads the file as it is',
        );
        expect(await driver.installed(), ['awgr-a']);
      },
    );

    test('keeps the tunnel\'s own socket out of the tunnel', () async {
      await driver.install(name: 'awgr-a', conf: mainConf);
      expect(bridge.protectedFds, [
        101,
      ], reason: 'the IPv4 socket; there is no IPv6 one');
    });

    test('a refusal to protect a socket is logged and not fatal', () async {
      bridge.protectWorks = false;
      await driver.install(name: 'awgr-a', conf: mainConf);
      expect(log.any((l) => l.contains('could not keep socket')), isTrue);
      expect(await driver.installed(), ['awgr-a']);
    });

    test(
      'stops tunnels nobody owns before it opens its own: a program that went '
      'away leaves them running, and two with one key spoil each other',
      () async {
        native.strays = 3;
        await driver.install(name: 'awgr-a', conf: mainConf);

        expect(native.calls.first, 'turnOffAll');
        expect(
          native.calls.indexOf('turnOffAll'),
          lessThan(native.calls.indexOf('turnOn')),
          reason: 'the strangers go before the new one starts',
        );
        expect(log.any((l) => l.contains('3 tunnel')), isTrue);
        expect(await driver.installed(), ['awgr-a']);
      },
    );

    test('does not touch the tunnels of its own when it opens a second one '
        '(a move to a new server keeps the old until the new works)', () async {
      await driver.install(name: 'awgr-a', conf: mainConf);
      native.calls.clear();
      await driver.install(name: 'awgr-b', conf: mainConf);

      expect(native.calls, ['turnOn']);
      expect(await driver.installed(), unorderedEquals(['awgr-a', 'awgr-b']));
    });

    test(
      'a tunnel that was brought up again is not mistaken for a stray',
      () async {
        await driver.install(name: 'awgr-a', conf: mainConf);
        await driver.uninstall('awgr-a');
        native.calls.clear();
        await driver.install(name: 'awgr-a', conf: mainConf);
        expect(native.calls.first, 'turnOffAll');
        expect(
          native.calls.where((c) => c.startsWith('turnOff ')),
          isEmpty,
          reason: 'only the library-wide sweep, nothing aimed at a handle',
        );
      },
    );

    group('when the network changes', () {
      test('the sockets are opened afresh and the system is told about the '
          'new ones', () async {
        await driver.install(name: 'awgr-a', conf: mainConf);
        bridge.protectedFds.clear();

        await driver.rebind('awgr-a');

        expect(native.rebound, [1]);
        expect(
          bridge.protectedFds,
          [native.sockets[1]![0]],
          reason: 'the descriptor after rebinding, not the one from before',
        );
        expect(log.any((l) => l.contains('reopened')), isTrue);
      });

      test('a tunnel that is not there is left alone', () async {
        await driver.rebind('awgr-a');
        expect(native.rebound, isEmpty);
      });

      test('sockets that could not be reopened are reported', () async {
        await driver.install(name: 'awgr-a', conf: mainConf);
        native.rebindResult = -2;
        native.error = 'open the sockets again: boom';
        await expectLater(
          driver.rebind('awgr-a'),
          throwsA(
            isA<TunnelException>().having(
              (e) => e.message,
              'message',
              contains('boom'),
            ),
          ),
        );
      });

      test('only this app\'s tunnel names are accepted', () async {
        await expectLater(
          driver.rebind('wg0'),
          throwsA(isA<TunnelException>()),
        );
      });
    });

    test('a handshake is a time, and "none yet" is null', () async {
      await driver.install(name: 'awgr-a', conf: mainConf);
      expect(await driver.latestHandshake('awgr-a'), isNull);
      native.handshakes[1] = 1_762_400_000;
      final t = await driver.latestHandshake('awgr-a');
      expect(t!.millisecondsSinceEpoch, 1_762_400_000 * 1000);
      expect(
        await driver.latestHandshake('awgr-b'),
        isNull,
        reason: 'not installed',
      );
    });

    test(
      'a library that cannot start says why, and lets the service go',
      () async {
        native.failWith = -3;
        native.error = 'configure the device: bad key';
        await expectLater(
          driver.install(name: 'awgr-a', conf: mainConf),
          throwsA(
            isA<TunnelException>().having(
              (e) => e.message,
              'message',
              contains('bad key'),
            ),
          ),
        );
        expect(await driver.installed(), isEmpty);
        expect(
          bridge.calls.last,
          'release',
          reason: 'a VPN service with nothing in it must not linger',
        );
      },
    );

    test('a system that will not open the interface is reported', () async {
      bridge.establishError = PlatformException(
        code: 'establish',
        message: 'revoked',
      );
      await expectLater(
        driver.install(name: 'awgr-a', conf: mainConf),
        throwsA(
          isA<TunnelException>().having(
            (e) => e.message,
            'message',
            contains('revoked'),
          ),
        ),
      );
      expect(
        native.calls,
        isNot(contains('turnOn')),
        reason: 'no interface, so nothing is started on it',
      );
    });

    test(
      'a file the system cannot use is refused before anything is opened',
      () async {
        await expectLater(
          driver.install(
            name: 'awgr-a',
            conf: '[Interface]\nAddress = 10.8.0.7/32\n',
          ),
          throwsA(isA<TunnelException>()),
        );
        expect(bridge.calls, isEmpty);
      },
    );

    test('installing the same name again stops the first one first', () async {
      await driver.install(name: 'awgr-a', conf: mainConf);
      await driver.install(name: 'awgr-a', conf: mainConf);
      expect(
        native.calls.where((c) => c != 'turnOffAll'),
        ['turnOn', 'turnOff 1', 'turnOn'],
        reason: 'the sweep for strays is a separate matter, tested above',
      );
      expect(await driver.installed(), ['awgr-a']);
    });

    test(
      'a move installs the new one first and keeps the service for it',
      () async {
        await driver.install(name: 'awgr-a', conf: mainConf);
        await driver.install(name: 'awgr-b', conf: mainConf);
        await driver.uninstall('awgr-a');

        expect(await driver.installed(), ['awgr-b']);
        expect(
          bridge.calls.where((c) => c == 'release'),
          isEmpty,
          reason: 'the service is still in use by the new tunnel',
        );
        expect(native.calls, contains('turnOff 1'));
      },
    );

    test('removing the last tunnel lets the service go', () async {
      await driver.install(name: 'awgr-a', conf: mainConf);
      await driver.uninstall('awgr-a');
      expect(bridge.calls.last, 'release');
      expect(await driver.installed(), isEmpty);
    });

    test('removing what is not there does nothing', () async {
      await driver.uninstall('awgr-a');
      expect(native.calls, isEmpty);
      expect(bridge.calls, isEmpty);
    });

    test('the enrollment tunnel comes and goes like any other', () async {
      await driver.install(name: 'awgr-enroll', conf: enrollConf);
      expect(bridge.established.single['routes'], ['10.8.0.1/32']);
      await driver.uninstall('awgr-enroll');
      expect(bridge.calls.last, 'release');
    });

    test('names that are not ours are refused', () async {
      for (final bad in ['x', 'awgr-', 'awgr-UPPER', '../awgr-a', 'awgr-a b']) {
        await expectLater(
          driver.install(name: bad, conf: mainConf),
          throwsA(isA<TunnelException>()),
          reason: bad,
        );
      }
      expect(bridge.calls, isEmpty);
    });
  });

  group('the rest of the system', () {
    test(
      'the name server is part of the interface: there is nothing to enable',
      () async {
        final g = AndroidDnsGuard();
        await g.enable('198.18.0.1');
        await g.disable();
      },
    );

    test('the device id comes from Android and is marked as such', () async {
      final bridge = FakeBridge();
      expect(await AndroidMachineId(bridge).machineId(), 'android:abc123');
      bridge.id = '';
      await expectLater(AndroidMachineId(bridge).machineId(), throwsStateError);
    });
  });

  group('the method channel', () {
    final binding = TestDefaultBinaryMessengerBinding.instance;
    const channel = MethodChannel('awg/vpn');
    final calls = <MethodCall>[];

    setUp(() {
      calls.clear();
      binding.defaultBinaryMessenger.setMockMethodCallHandler(channel, (
        call,
      ) async {
        calls.add(call);
        switch (call.method) {
          case 'establish':
            return 77;
          case 'prepare':
            return true;
          case 'androidId':
            return 'dev-1';
          case 'notifications':
          case 'battery':
          case 'install':
            return 'granted';
          default:
            return null;
        }
      });
    });

    tearDown(() {
      binding.defaultBinaryMessenger.setMockMethodCallHandler(channel, null);
    });

    test(
      'hands the interface description over under the names Kotlin reads',
      () async {
        final bridge = MethodChannelVpnBridge();
        final fd = await bridge.establish(
          session: 'Rebirth',
          addresses: ['10.8.0.7/32'],
          routes: ['198.18.0.0/15'],
          dns: ['198.18.0.1'],
          mtu: 1280,
        );
        expect(fd, 77);
        expect(calls.single.method, 'establish');
        expect(calls.single.arguments, {
          'session': 'Rebirth',
          'addresses': ['10.8.0.7/32'],
          'routes': ['198.18.0.0/15'],
          'dns': ['198.18.0.1'],
          'mtu': 1280,
        });
      },
    );

    test(
      'asks for the permission, the device id and protection by the same names',
      () async {
        final bridge = MethodChannelVpnBridge();
        expect(await bridge.prepare(), isTrue);
        expect(await bridge.androidId(), 'dev-1');
        await bridge.protect(9);
        await bridge.release();
        await bridge.setStatus('Подключено · Амстердам');
        expect(calls.map((c) => c.method), [
          'prepare',
          'androidId',
          'protect',
          'release',
          'status',
        ]);
        expect(calls[2].arguments, {'fd': 9});
        expect(calls.last.arguments, {'text': 'Подключено · Амстердам'});
      },
    );

    test(
      'asks the start-up questions by the names Kotlin answers to',
      () async {
        final bridge = MethodChannelVpnBridge();
        expect(await bridge.ensureNotifications(), 'granted');
        expect(await bridge.ensureBatteryExemption(), 'granted');
        expect(await bridge.ensureInstallPermission(), 'granted');
        expect(calls.map((c) => c.method), [
          'notifications',
          'battery',
          'install',
        ]);
      },
    );

    test('takes a missing answer as a refusal', () async {
      binding.defaultBinaryMessenger.setMockMethodCallHandler(
        channel,
        (call) async => null,
      );
      final bridge = MethodChannelVpnBridge();
      expect(await bridge.ensureNotifications(), 'denied');
      expect(await bridge.ensureBatteryExemption(), 'denied');
      expect(await bridge.ensureInstallPermission(), 'denied');
    });

    test('hears when the system takes the VPN away', () async {
      final bridge = MethodChannelVpnBridge();
      var revoked = 0;
      bridge.onRevoked = () => revoked++;
      await binding.defaultBinaryMessenger.handlePlatformMessage(
        'awg/vpn',
        const StandardMethodCodec().encodeMethodCall(
          const MethodCall('revoked'),
        ),
        (_) {},
      );
      expect(revoked, 1);
    });
  });
}
