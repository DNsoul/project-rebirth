import 'dart:async';

import 'package:awg_client/core/app_state.dart';
import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/gist_source.dart' show GistSnapshot;
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

/// Everything one test needs, wired together.
class Rig {
  Rig._(this.store);

  static Future<Rig> create({PersistedState? saved}) async {
    final rig = Rig._(MemoryStateStore(saved));
    rig.tunnel = FakeTunnel(rig.fleet, rig.clock);
    rig.probe = FakeProbe(rig.tunnel);
    rig.controller = ConnectionController(
      bootstrap: testBootstrap(),
      gist: rig.gist,
      store: rig.store,
      tunnel: rig.tunnel,
      dns: rig.dns,
      probe: rig.probe,
      machine: FakeMachine(),
      transportFactory: () =>
          rig.transport = FakeTransport(rig.tunnel, rig.clock),
      deviceName: 'test-pc',
      now: rig.clock.now,
      sleep: rig.clock.sleep,
      runTimers: false,
      log: rig.logs.add,
    );
    return rig;
  }

  final MemoryStateStore store;
  final clock = FakeClock();
  final fleet = FakeFleet();
  final gist = FakeGist();
  final dns = FakeDns();
  final logs = <String>[];
  late final FakeTunnel tunnel;
  late final FakeProbe probe;
  late final ConnectionController controller;
  FakeTransport? transport;
  int _seq = 0;

  /// Publishes [s] as the current server, as its manager does.
  Future<void> publish(
    FakeServer s, {
    int? seq,
    int epoch = 3,
    String? body,
  }) async {
    gist.publish(
      config: await sealedConfig(
        body ??
            bodyJson(
              endpoint: s.endpoint,
              serverByte: s.keyByte,
              region: s.region,
            ),
        seq: seq ?? ++_seq,
        at: clock.now(),
        epoch: epoch,
      ),
    );
  }

  AppStatus get status => controller.status;

  /// The tunnels installed right now, in the order they were made.
  List<String> get tunnels => tunnel.ours.toList();
}

/// Lets the app run until [condition] holds. The tests stop it at a known point
/// (inside the install, inside the web request) and then press something.
Future<void> pumpUntil(bool Function() condition, String what) async {
  for (var i = 0; i < 1000; i++) {
    if (condition()) return;
    await Future<void>.delayed(Duration.zero);
  }
  fail('never got to: $what');
}

void main() {
  late Rig rig;
  late FakeServer sto;

  setUp(() async {
    rig = await Rig.create();
    sto = rig.fleet.add('203.0.113.9:41820', 11, region: 'sto');
    await rig.publish(sto);
  });

  group('the first connection', () {
    test('registers the device, then connects with its own key', () async {
      await rig.controller.connect();

      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(rig.status.region, 'sto');
      expect(sto.devices, hasLength(1));
      expect(sto.devices.values.single.address, '10.8.0.2/32');

      // The enrollment tunnel came first and is gone; the real one remains.
      expect(rig.tunnel.log, [
        'install awgr-enroll',
        'uninstall awgr-enroll',
        'install awgr-a',
      ]);
      expect(rig.tunnels, ['awgr-a']);
      expect(rig.tunnel.carries('awgr-a'), isTrue);
    });

    test(
      'the enrollment tunnel can reach the registration service and nothing else',
      () async {
        var checked = false;
        final original = rig.tunnel;
        // Look at it while it exists: it is removed again by the time connect() returns.
        final watcher = Timer.periodic(Duration.zero, (_) {
          final ips = original.allowedIpsOf('awgr-enroll');
          if (ips != null) {
            checked = true;
            expect(ips, '10.8.0.1/32');
            expect(original.confOf('awgr-enroll'), isNot(contains('DNS')));
            expect(
              original.confOf('awgr-enroll'),
              contains('PrivateKey = ${encodeStd(sto.enrollPrivateKey)}'),
            );
          }
        });
        await rig.controller.connect();
        watcher.cancel();
        expect(
          checked || rig.tunnel.log.contains('install awgr-enroll'),
          isTrue,
        );
      },
    );

    test('the machine asks only the server for names once it is up', () async {
      await rig.controller.connect();
      expect(rig.dns.enabledFor, '198.18.0.1');
      expect(
        rig.probe.asked,
        isNotEmpty,
        reason: 'the whole path was checked, not just the handshake',
      );
    });

    test('what it learned is remembered', () async {
      await rig.controller.connect();
      final saved = await rig.store.load();
      expect(saved.deviceKey, isNotNull);
      expect(saved.lastAddress, '10.8.0.2/32');
      expect(saved.addresses[encodeStd(sto.publicKey)], '10.8.0.2/32');
      expect(saved.proto.lastSeq, 1);
      expect(saved.configBody, contains('203.0.113.9:41820'));
    });

    test('a registration datagram that is lost is simply sent again', () async {
      final r = await Rig.create();
      final s = r.fleet.add('198.51.100.1:5000', 21);
      await r.publish(s);
      // The transport is made inside connect(), so lose packets from the start.
      var made = 0;
      final lossy = ConnectionController(
        bootstrap: testBootstrap(),
        gist: r.gist,
        store: r.store,
        tunnel: r.tunnel,
        dns: r.dns,
        probe: r.probe,
        machine: FakeMachine(),
        transportFactory: () => FakeTransport(r.tunnel, r.clock)
          ..drop = 3
          ..let(() => made++),
        now: r.clock.now,
        sleep: r.clock.sleep,
        runTimers: false,
      );
      await lossy.connect();
      expect(lossy.status.phase, Phase.connected, reason: lossy.status.message);
      expect(made, 1);
    });
  });

  group('reconnecting', () {
    test('a device the server already knows does not register again', () async {
      await rig.controller.connect();
      await rig.controller.disconnect();
      rig.tunnel.log.clear();

      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(rig.tunnel.log, [
        'install awgr-a',
      ], reason: 'no enrollment tunnel the second time');
      expect(sto.devices, hasLength(1));
    });

    test(
      'disconnecting removes the tunnel and puts the machine\'s names back',
      () async {
        await rig.controller.connect();
        await rig.controller.disconnect();
        expect(rig.status.phase, Phase.idle);
        expect(rig.tunnels, isEmpty);
        expect(rig.dns.enabledFor, isNull);
        expect(rig.dns.log.last, 'disable');
      },
    );

    test('a server that lost the device is told about it again', () async {
      await rig.controller.connect();
      await rig.controller.disconnect();
      sto.devices.clear(); // restarted from an empty list

      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(sto.devices, hasLength(1));
      expect(rig.tunnel.log, contains('install awgr-enroll'));
    });
  });

  group('moving to a new server', () {
    late FakeServer waw;

    setUp(() async {
      await rig.controller.connect();
      waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw');
    });

    test(
      'a successor that took the devices over is joined without registering again',
      () async {
        waw.inherit(sto);
        await rig.publish(waw);
        rig.tunnel.log.clear();

        await rig.controller.pollNow();

        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
        expect(rig.status.region, 'waw');
        expect(
          rig.tunnel.log,
          ['install awgr-b', 'uninstall awgr-a'],
          reason:
              'the new tunnel first, the old one only once the new one works',
        );
        expect(rig.tunnels, ['awgr-b']);
        expect(rig.dns.enabledFor, '198.18.0.1');
        expect(
          rig.dns.log.where((l) => l == 'disable').length,
          lessThanOrEqualTo(1),
          reason:
              'names are never handed back to the ordinary resolver in the middle of a move',
        );
      },
    );

    test(
      'a successor that has never heard of the device gets it registered',
      () async {
        await rig.publish(waw);
        rig.tunnel.log.clear();

        await rig.controller.pollNow();

        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
        expect(waw.devices, hasLength(1));
        // The old tunnel stays through the registration, and goes at the end.
        expect(
          rig.tunnel.log.first,
          'install awgr-b',
          reason: 'tries the address it already had first',
        );
        expect(rig.tunnel.log, contains('install awgr-enroll'));
        expect(rig.tunnel.log.last, 'uninstall awgr-a');
        expect(rig.tunnels, ['awgr-b']);
      },
    );

    test(
      'a successor that knows the key under another address is not trusted on the handshake alone',
      () async {
        waw.inherit(sto);
        // Same key, different address on record: the handshake works, nothing is carried.
        final entry = waw.devices.entries.single;
        waw.devices[entry.key] = (id: entry.value.id, address: '10.8.0.99/32');
        await rig.publish(waw);

        await rig.controller.pollNow();

        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
        expect(
          rig.tunnel.carries(rig.tunnels.single),
          isTrue,
          reason: 'ends up with the address the server has',
        );
        expect(
          rig.tunnel.confOf(rig.tunnels.single),
          contains('Address = 10.8.0.99/32'),
        );
      },
    );

    test(
      'a new server that does not answer leaves the old one in place',
      () async {
        waw.alive = false;
        await rig.publish(waw);

        await rig.controller.pollNow();

        expect(rig.status.phase, Phase.connected);
        expect(rig.status.region, 'sto');
        expect(rig.status.note, contains('остаёмся на прежнем'));
        expect(rig.tunnels, ['awgr-a']);
        expect(rig.tunnel.carries('awgr-a'), isTrue);
      },
    );

    test('and tries the new one again, but not at once', () async {
      waw.alive = false;
      await rig.publish(waw);
      await rig.controller.pollNow();
      final attempts = rig.tunnel.log
          .where((l) => l == 'install awgr-b')
          .length;

      rig.clock.advance(const Duration(seconds: 20));
      await rig.controller.pollNow();
      expect(
        rig.tunnel.log.where((l) => l == 'install awgr-b').length,
        attempts,
        reason: 'too soon',
      );

      waw.alive = true;
      rig.clock.advance(const Duration(minutes: 2));
      await rig.controller.pollNow();
      expect(rig.status.region, 'waw', reason: rig.status.message);
      expect(rig.status.note, isEmpty);
      expect(rig.tunnels, ['awgr-b']);
    });

    test(
      'nothing happens while the published server is the one in use',
      () async {
        rig.tunnel.log.clear();
        await rig.controller.pollNow();
        await rig.controller.pollNow();
        expect(rig.tunnel.log, isEmpty);
        expect(
          rig.gist.notModified,
          greaterThan(0),
          reason: 'asked with the etag, answered "unchanged"',
        );
      },
    );

    test('moves back and forth between the two slots', () async {
      waw.inherit(sto);
      await rig.publish(waw);
      await rig.controller.pollNow();
      expect(rig.tunnels, ['awgr-b']);

      final fra = rig.fleet.add('192.0.2.50:4000', 41, region: 'fra')
        ..inherit(sto);
      await rig.publish(fra);
      await rig.controller.pollNow();
      expect(rig.tunnels, ['awgr-a']);
      expect(rig.status.region, 'fra');
    });
  });

  group('what is published has to hold up', () {
    test('a configuration signed by the wrong key is not used', () async {
      rig.gist.publish(
        config: await sealedConfig(
          bodyJson(endpoint: '203.0.113.66:1', serverByte: 66),
          seq: 99,
          at: rig.clock.now(),
          signWith: seed('root_seed'), // a key the certificate does not name
        ),
      );
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.status.message, contains('не прошли проверку'));
      expect(rig.tunnels, isEmpty);
    });

    test('and the last good one is used instead, if there is one', () async {
      await rig.controller.connect();
      await rig.controller.disconnect();

      rig.gist.publish(
        config: await sealedConfig(
          bodyJson(endpoint: '203.0.113.66:1', serverByte: 66),
          seq: 99,
          at: rig.clock.now(),
          signWith: seed('root_seed'),
        ),
      );
      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(
        rig.tunnel.confOf('awgr-a'),
        contains('203.0.113.9:41820'),
        reason: 'the old, good server',
      );
    });

    test('an older publication cannot take the place of a newer one', () async {
      await rig.publish(sto, seq: 5);
      await rig.controller.connect();
      await rig.controller.disconnect();

      // The operator's gist is replayed with an earlier, correctly signed document.
      final old = rig.fleet.add('192.0.2.1:1', 51);
      await rig.publish(old, seq: 3);
      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(rig.tunnel.confOf('awgr-a'), contains('203.0.113.9:41820'));
    });

    test(
      'a revocation cuts off a server certified below it, even from the saved copy',
      () async {
        await rig.controller.connect();
        await rig.controller.disconnect();

        rig.gist.publish(
          revocation: await sealedRevocation(4, rig.clock.now()),
        );
        await rig.controller.connect();
        expect(
          rig.status.phase,
          Phase.failed,
          reason: 'the only configuration on offer is from epoch 3',
        );
        expect(rig.tunnels, isEmpty);

        // A server certified at the new epoch is fine.
        final fresh = rig.fleet.add('192.0.2.77:4000', 71);
        await rig.publish(fresh, epoch: 4);
        await rig.controller.connect();
        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      },
    );

    test(
      'the gist being unreachable is survivable with a saved configuration',
      () async {
        await rig.controller.connect();
        await rig.controller.disconnect();

        rig.gist.down = true;
        await rig.controller.connect();
        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      },
    );

    test('and an honest failure without one', () async {
      final fresh = await Rig.create();
      fresh.gist.down = true;
      await fresh.controller.connect();
      expect(fresh.status.phase, Phase.failed);
      expect(fresh.status.message, contains('GitHub'));
    });
  });

  group('failures leave nothing behind', () {
    test('a refused registration', () async {
      sto.refuse = enrollFull;
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.status.message, contains('нет мест для новых устройств'));
      expect(rig.tunnels, isEmpty);
      expect(rig.dns.enabledFor, isNull);
    });

    test('a server that never answers', () async {
      sto.alive = false;
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.tunnels, isEmpty);
      expect(rig.dns.enabledFor, isNull);
    });

    test('a server too old to register devices', () async {
      await rig.publish(
        sto,
        body: bodyJson(
          endpoint: sto.endpoint,
          serverByte: sto.keyByte,
          version: 1,
        ),
      );
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.status.message, contains('старой версии'));
      expect(rig.tunnels, isEmpty);
    });

    test(
      'a machine that cannot run the tunnel says why, and does nothing',
      () async {
        rig.tunnel.problem =
            'Нужно запустить приложение от имени администратора.';
        await rig.controller.connect();
        expect(rig.status.phase, Phase.failed);
        expect(rig.status.message, contains('администратора'));
        expect(rig.tunnel.log, isEmpty);
        expect(
          rig.dns.log,
          isEmpty,
          reason: 'nothing was started, so there is nothing to take back',
        );
      },
    );

    test('the failure shows at once, not after the clean-up', () async {
      // Removing the tunnel takes a moment on a real machine. When something has
      // been started it has to be undone, but the person is told what is going
      // on instead of waiting for the verdict.
      await rig.controller.start();
      rig.probe.answers = false; // the tunnel comes up but carries nothing
      final said = <String>[];
      rig.tunnel.onUninstall = (name) {
        if (name != 'awgr-enroll') said.add(rig.status.message);
      };
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.tunnels, isEmpty);
      expect(
        said,
        isNotEmpty,
        reason: 'the connection tunnel is taken down after the failure',
      );
      expect(
        said,
        everyElement(contains('Убираю')),
        reason: 'the screen says what it is doing while the clean-up runs',
      );
    });
  });

  group('the start of the app', () {
    test(
      'removes what a crashed run left, and nothing that is not its own',
      () async {
        await rig.controller.connect();
        rig.tunnel.foreign.add('AmneziaVPN'); // somebody else\'s VPN
        rig.tunnel.log.clear();

        // A new process starts with whatever the last one left.
        final again = ConnectionController(
          bootstrap: testBootstrap(),
          gist: rig.gist,
          store: rig.store,
          tunnel: rig.tunnel,
          dns: rig.dns,
          probe: rig.probe,
          machine: FakeMachine(),
          transportFactory: () => FakeTransport(rig.tunnel, rig.clock),
          now: rig.clock.now,
          sleep: rig.clock.sleep,
          runTimers: false,
        );
        await again.start();

        expect(rig.tunnel.log, ['uninstall awgr-a']);
        expect(rig.tunnel.log, isNot(contains('uninstall AmneziaVPN')));
        expect(rig.dns.enabledFor, isNull);
        expect(again.status.phase, Phase.idle);
      },
    );

    test(
      'reports a machine that cannot run the tunnel before anyone presses anything',
      () async {
        rig.tunnel.problem = 'Не найден AmneziaWG.';
        await rig.controller.start();
        expect(rig.status.phase, Phase.idle);
        expect(rig.status.note, contains('AmneziaWG'));
        expect(
          rig.tunnel.log,
          isEmpty,
          reason: 'nothing to clean up with a tunnel it cannot run',
        );
        expect(rig.dns.log, isEmpty);
      },
    );

    test('is not stopped by a cleanup that fails', () async {
      final c = ConnectionController(
        bootstrap: testBootstrap(),
        gist: rig.gist,
        store: rig.store,
        tunnel: rig.tunnel,
        dns: FakeDnsThatFails(),
        probe: rig.probe,
        machine: FakeMachine(),
        transportFactory: () => FakeTransport(rig.tunnel, rig.clock),
        now: rig.clock.now,
        sleep: rig.clock.sleep,
        runTimers: false,
      );
      await c.start(); // must not throw: the app would not open at all
      expect(c.status.phase, Phase.idle);
    });
  });

  group('hearing about a new version', () {
    ConnectionController another() => ConnectionController(
      bootstrap: testBootstrap(),
      gist: rig.gist,
      store: rig.store,
      tunnel: rig.tunnel,
      dns: rig.dns,
      probe: rig.probe,
      machine: FakeMachine(),
      transportFactory: () => FakeTransport(rig.tunnel, rig.clock),
      now: rig.clock.now,
      sleep: rig.clock.sleep,
      runTimers: false,
      log: rig.logs.add,
    );

    test('is silent when nothing has been announced', () async {
      await rig.controller.checkForUpdate();
      expect(rig.controller.release, isNull);
    });

    test('finds the announcement without a connection', () async {
      rig.gist.publish(release: await sealedRelease());
      final heard = rig.controller.releaseStream.first;

      await rig.controller.checkForUpdate();

      expect(rig.controller.release?.build, 12);
      expect(rig.controller.release?.version, '1.2.0');
      expect((await heard).build, 12);
      expect(rig.tunnels, isEmpty, reason: 'looking is not connecting');
      final saved = await rig.store.load();
      expect(saved.proto.lastBuild, 12);
      expect(saved.releaseBlob, isNotNull);
    });

    test('keeps the offer across a restart, even with no network', () async {
      rig.gist.publish(release: await sealedRelease());
      await rig.controller.checkForUpdate();

      rig.gist.down = true;
      final again = another();
      await again.start();
      expect(again.release?.build, 12);
    });

    test(
      'does not trust a saved announcement that no longer verifies',
      () async {
        rig.gist.publish(release: await sealedRelease());
        await rig.controller.checkForUpdate();
        final saved = await rig.store.load();
        saved.releaseBlob = await forgedRelease();
        await rig.store.save(saved);

        final again = another();
        await again.start();
        expect(again.release, isNull);
      },
    );

    test('ignores an announcement signed by the wrong key', () async {
      rig.gist.publish(release: await forgedRelease());
      await rig.controller.checkForUpdate();

      expect(rig.controller.release, isNull);
      expect(rig.logs.join('\n'), contains('refused'));
      expect((await rig.store.load()).proto.lastBuild, 0);
    });

    test('ignores an older announcement once a newer one was taken', () async {
      rig.gist.publish(release: await sealedRelease(build: 12));
      await rig.controller.checkForUpdate();

      rig.gist.publish(
        release: await sealedRelease(build: 11, version: '1.1.0'),
      );
      await rig.controller.checkForUpdate();

      expect(rig.controller.release?.build, 12);
      expect((await rig.store.load()).proto.lastBuild, 12);
    });

    test('takes a newer one over an older one', () async {
      rig.gist.publish(release: await sealedRelease(build: 12));
      await rig.controller.checkForUpdate();
      rig.gist.publish(
        release: await sealedRelease(build: 13, version: '1.3.0'),
      );
      await rig.controller.checkForUpdate();
      expect(rig.controller.release?.version, '1.3.0');
    });

    test('is not undone by a new configuration read later', () async {
      // The remembered build lives next to the remembered configuration number;
      // saving one must not lose the other.
      rig.gist.publish(release: await sealedRelease());
      await rig.controller.checkForUpdate();

      final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw')
        ..inherit(sto);
      await rig.publish(waw);
      // A gist that no longer carries the announcement, so nothing in what is
      // read next could put the number back.
      final now = rig.gist.current!;
      rig.gist.current = GistSnapshot(
        config: now.config,
        revocation: now.revocation,
        etag: 'etag-without-announcement',
      );
      await rig.controller.connect();

      final saved = await rig.store.load();
      expect(saved.proto.lastBuild, 12);
      expect(saved.proto.lastSeq, greaterThan(0));
    });

    test('is also noticed by the regular look for a new server', () async {
      await rig.controller.connect();
      rig.gist.publish(
        release: await sealedRelease(build: 14, version: '1.4.0'),
      );
      await rig.controller.pollNow();
      expect(rig.controller.release?.build, 14);
      expect(
        rig.status.phase,
        Phase.connected,
        reason: 'the tunnel is left alone',
      );
    });

    test(
      'does not let a saved "unchanged" hide an announcement never read',
      () {
        // Written by a version that did not know about announcements.
        final old = PersistedState.fromJson({
          'etag': 'etag-1',
          'config_epoch': 3,
        });
        expect(old.etag, isNull);

        final now = PersistedState.fromJson(
          PersistedState(etag: 'etag-2').toJson(),
        );
        expect(now.etag, 'etag-2');
      },
    );
  });

  group('noticing that the server has gone quiet', () {
    test('says so, and looks for a replacement at once', () async {
      await rig.controller.connect();
      final fetched = rig.gist.fetches;

      rig.tunnel.handshakeAge = const Duration(minutes: 5);
      await rig.controller.checkHealthNow();
      expect(rig.status.lostContact, isTrue);
      expect(
        rig.gist.fetches,
        greaterThan(fetched),
        reason: 'a silent server is probably a replaced one',
      );

      rig.tunnel.handshakeAge = null;
      await rig.controller.checkHealthNow();
      expect(rig.status.lostContact, isFalse);
    });

    test('and follows a replacement that is already published', () async {
      await rig.controller.connect();
      final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw')
        ..inherit(sto);
      await rig.publish(waw);
      sto.alive = false;

      rig.clock.advance(const Duration(minutes: 5)); // the handshake is now old
      await rig.controller.checkHealthNow();
      expect(rig.status.region, 'waw', reason: rig.status.message);
      expect(rig.status.lostContact, isFalse);
    });
  });

  // The app has to answer a press at once, say what it is doing, and let go of a
  // slow step the moment the person changes their mind. These stop the app
  // halfway through (at the tunnel, at the web request) and press something.
  group('responsiveness', () {
    test('the screen answers before anything slow has started', () async {
      final connecting = rig.controller.connect();
      expect(
        rig.status.phase,
        Phase.connecting,
        reason: 'not after the first await: that is what feels like a freeze',
      );
      await connecting;
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);

      final disconnecting = rig.controller.disconnect();
      expect(rig.status.phase, Phase.disconnecting);
      expect(rig.status.message, isNotEmpty);
      await disconnecting;
      expect(rig.status.phase, Phase.idle);
    });

    test('says each step it takes, in the order it takes them', () async {
      final seen = <String>[];
      final sub = rig.controller.statusStream.listen((s) {
        if (s.message.isNotEmpty && (seen.isEmpty || seen.last != s.message)) {
          seen.add(s.message);
        }
      });
      await rig.controller.connect();
      await sub.cancel();

      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(seen, [
        'Готовлюсь…',
        'Читаю настройки…',
        'Регистрирую это устройство…',
        'Связываюсь с сервером…',
        'Регистрирую это устройство…',
        'Поднимаю туннель…',
        'Жду ответа сервера…',
        'Проверяю, что трафик идёт…',
        'Включаю защиту имён…',
      ]);
    });

    test(
      'names the stage it is at, and never goes back to an earlier one',
      () async {
        final steps = <ConnectStep?>[];
        final sub = rig.controller.statusStream.listen((s) {
          if (s.phase == Phase.connecting) {
            steps.add(s.step);
          }
        });
        await rig.controller.connect();
        await sub.cancel();

        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
        expect(steps, isNotEmpty);
        expect(steps, everyElement(isNotNull));
        for (var i = 1; i < steps.length; i++) {
          expect(
            steps[i]!.index,
            greaterThanOrEqualTo(steps[i - 1]!.index),
            reason: 'step $i of $steps',
          );
        }
        expect(steps.toSet().toList(), ConnectStep.values);
        expect(rig.status.step, isNull, reason: 'once it is up, no stage');
      },
    );

    test('says at which stage it broke', () async {
      final fresh = await Rig.create();
      fresh.gist.down = true;
      await fresh.controller.connect();
      expect(fresh.status.phase, Phase.failed);
      expect(fresh.status.step, ConnectStep.settings);

      sto.alive = false;
      await rig.controller.connect();
      expect(rig.status.phase, Phase.failed);
      expect(rig.status.step, ConnectStep.server);
    });

    test('and says where a move is going', () async {
      await rig.controller.connect();
      final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw')
        ..inherit(sto);
      await rig.publish(waw);

      final seen = <String>[];
      final sub = rig.controller.statusStream.listen((s) {
        if (s.message.isNotEmpty) seen.add(s.message);
      });
      await rig.controller.pollNow();
      await sub.cancel();

      expect(seen.first, 'Перехожу на новый сервер: Варшава…');
      expect(seen, contains('Варшава: Поднимаю туннель…'));
      expect(seen, contains('Варшава: Жду ответа сервера…'));
    });

    test('"disconnect" in the middle of connecting stops it at once', () async {
      rig.tunnel.installGate = Completer<void>();
      final phases = <Phase>[];
      final sub = rig.controller.statusStream.listen(
        (s) => phases.add(s.phase),
      );

      final connecting = rig.controller.connect();
      await pumpUntil(() => rig.tunnel.installWaiting, 'the install to start');

      final disconnecting = rig.controller.disconnect();
      expect(
        rig.status.phase,
        Phase.disconnecting,
        reason: 'the screen says so before the step in progress has finished',
      );

      rig.tunnel.installGate!.complete();
      await disconnecting;
      await connecting;
      await sub.cancel();

      expect(rig.status.phase, Phase.idle);
      expect(phases, isNot(contains(Phase.connected)));
      expect(phases, isNot(contains(Phase.failed)));
      expect(rig.tunnels, isEmpty, reason: 'nothing left running');
      expect(rig.dns.enabledFor, isNull);
    });

    test('"cancel" calls a connection off: idle, not an error', () async {
      rig.tunnel.installGate = Completer<void>();
      final connecting = rig.controller.connect();
      await pumpUntil(() => rig.tunnel.installWaiting, 'the install to start');

      await rig.controller.cancel();
      expect(rig.status.message, 'Отменяю…');

      rig.tunnel.installGate!.complete();
      await connecting;
      expect(rig.status.phase, Phase.idle, reason: rig.status.message);
      expect(rig.status.message, isEmpty);
      expect(rig.tunnels, isEmpty);
      expect(
        sto.devices,
        isEmpty,
        reason: 'it never got as far as registering',
      );
    });

    test(
      'a web request that never answers does not hold anything up',
      () async {
        rig.gist.fetchGate =
            Completer<void>(); // never completed during the test
        final connecting = rig.controller.connect();
        await pumpUntil(() => rig.gist.fetchWaiting, 'the request to start');

        await rig.controller.cancel();
        await connecting.timeout(
          const Duration(seconds: 2),
          onTimeout: () => fail('still waiting for the request after "cancel"'),
        );
        expect(rig.status.phase, Phase.idle);
        rig.gist.fetchGate!.complete(); // let it go, so nothing dangles
      },
    );

    test(
      '"disconnect" does not queue behind a poll waiting on the network',
      () async {
        await rig.controller.connect();
        rig.gist.fetchGate = Completer<void>();
        final poll = rig.controller.pollNow();
        await pumpUntil(() => rig.gist.fetchWaiting, 'the poll to start');

        await rig.controller.disconnect().timeout(
          const Duration(seconds: 2),
          onTimeout: () => fail('disconnect waited for the poll'),
        );
        expect(rig.status.phase, Phase.idle);
        expect(rig.tunnels, isEmpty);

        rig.gist.fetchGate!.complete();
        await poll;
        expect(
          rig.status.phase,
          Phase.idle,
          reason: 'the poll did not wake anything up',
        );
      },
    );

    test(
      'calling off a move leaves the old server in use, and the new tunnel gone',
      () async {
        await rig.controller.connect();
        final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw')
          ..inherit(sto);
        await rig.publish(waw);

        rig.tunnel.installGate = Completer<void>();
        final moving = rig.controller.pollNow();
        await pumpUntil(() => rig.tunnel.installWaiting, 'the move to start');
        expect(rig.status.phase, Phase.switching);

        await rig.controller.cancel();
        rig.tunnel.installGate!.complete();
        await moving;

        expect(rig.status.phase, Phase.connected);
        expect(rig.status.region, 'sto');
        expect(rig.status.note, contains('отменён'));
        expect(rig.tunnels, [
          'awgr-a',
        ], reason: 'the old one stays, the new one is removed');
        expect(rig.tunnel.carries('awgr-a'), isTrue);
      },
    );

    test(
      'the names and the tunnel come down side by side, not one after the other',
      () async {
        await rig.controller.connect();
        // On a real machine taking the name rule away is a second of PowerShell.
        rig.dns.disableGate = Completer<void>();
        final disconnecting = rig.controller.disconnect();

        await pumpUntil(() => rig.tunnels.isEmpty, 'the tunnel to be removed');
        expect(
          rig.status.phase,
          Phase.disconnecting,
          reason: 'the names are still being given back',
        );
        expect(rig.dns.enabledFor, isNotNull);

        rig.dns.disableGate!.complete();
        await disconnecting;
        expect(rig.status.phase, Phase.idle);
        expect(rig.dns.enabledFor, isNull);
      },
    );

    test('the name rule is only touched when there may be one', () async {
      await rig.controller.start(); // unknown after a crash: looked at once
      expect(rig.dns.log, ['disable']);

      sto.alive = false;
      await rig.controller.connect(); // fails, and had put nothing in place
      expect(rig.status.phase, Phase.failed);
      expect(
        rig.dns.log,
        ['disable'],
        reason: 'each look costs about a second, so none is made for nothing',
      );
    });

    test(
      'and is given back after a connection that did put it in place',
      () async {
        await rig.controller.connect();
        expect(rig.dns.enabledFor, isNotNull);
        rig.dns.log.clear();
        await rig.controller.disconnect();
        expect(rig.dns.log, ['disable']);
      },
    );

    test('cancelling when nothing is in progress does nothing', () async {
      await rig.controller.cancel();
      expect(rig.status.phase, Phase.idle);

      await rig.controller.connect();
      await rig.controller.cancel();
      expect(
        rig.status.phase,
        Phase.connected,
        reason: 'that is what "disconnect" is for',
      );
      expect(rig.tunnels, ['awgr-a']);
    });

    test(
      'a second press while connecting is not a second connection',
      () async {
        final first = rig.controller.connect();
        final second = rig.controller.connect();
        await Future.wait([first, second]);
        expect(rig.status.phase, Phase.connected, reason: rig.status.message);
        expect(
          rig.tunnel.log.where((l) => l == 'install awgr-enroll'),
          hasLength(1),
        );
        expect(sto.devices, hasLength(1));
      },
    );

    test(
      'leaving the app in the middle of connecting leaves nothing behind',
      () async {
        rig.tunnel.installGate = Completer<void>();
        final connecting = rig.controller.connect();
        await pumpUntil(
          () => rig.tunnel.installWaiting,
          'the install to start',
        );

        final leaving = rig.controller.dispose();
        rig.tunnel.installGate!.complete();
        await leaving;
        await connecting;
        expect(rig.tunnels, isEmpty);
        expect(rig.dns.enabledFor, isNull);
      },
    );

    test(
      'the log says how long each step took, so a slow one can be found',
      () async {
        await rig.controller.connect();
        await rig.controller.disconnect();
        final log = rig.logs.join('\n');
        for (final part in [
          RegExp(r'connect: check of the system \d+ ms'),
          RegExp(r'connect: settings \d+ ms'),
          RegExp(r'connect: tunnel \d+ ms'),
          RegExp(r'connect: \d+ ms'),
          RegExp(r'disconnect: tunnel and names \d+ ms'),
          RegExp(r'disconnect: \d+ ms'),
        ]) {
          expect(part.hasMatch(log), isTrue, reason: '$part in:\n$log');
        }
      },
    );
  });

  group('one device, one identity', () {
    test('the key is made once and reused', () async {
      await rig.controller.connect();
      final first = (await rig.store.load()).deviceKey;
      await rig.controller.disconnect();
      await rig.controller.connect();
      expect((await rig.store.load()).deviceKey, first);
    });

    test(
      'the server sees the device\'s own identity and the name the app gave',
      () async {
        await rig.controller.connect();
        final id = sto.devices.values.single.id;
        expect(decode(id), await deviceIdFrom('machine-guid-1'));
      },
    );
  });
}

extension _Let<T> on T {
  void let(void Function() f) => f();
}
