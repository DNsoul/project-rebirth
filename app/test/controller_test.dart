import 'dart:async';

import 'package:awg_client/core/app_state.dart';
import 'package:awg_client/core/controller.dart';
import 'package:awg_client/core/gist_source.dart'
    show GistSnapshot, GistSource, MirroredSource;
import 'package:awg_client/core/platform.dart' show TunnelException;
import 'package:awg_proto/awg_proto.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fakes.dart';

/// Everything one test needs, wired together.
class Rig {
  Rig._(this.store);

  /// With [wrap], what the app reads from is made out of the rig's own gist, so
  /// that a test can put other places beside it.
  static Future<Rig> create({
    PersistedState? saved,
    GistSource Function(FakeGist gist)? wrap,
    Stream<bool>? networkChanges,
  }) async {
    final rig = Rig._(MemoryStateStore(saved));
    rig.tunnel = FakeTunnel(rig.fleet, rig.clock);
    rig.probe = FakeProbe(rig.tunnel);
    rig.controller = ConnectionController(
      bootstrap: testBootstrap(),
      gist: wrap?.call(rig.gist) ?? rig.gist,
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
      networkChanges: networkChanges,
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

    // A computer saw "maybe it has already been replaced" for forty minutes while
    // the server was in its place and healthy (2026-11-12 logs).
    test('a server that took the device but carries nothing is said to be in '
        'its place, and why is written down', () async {
      rig.probe.answers = false;
      await rig.controller.connect();

      expect(rig.status.phase, Phase.failed);
      expect(rig.status.message, contains('Сервер на месте'));
      expect(rig.status.message, contains('другой VPN'));
      expect(
        rig.logs,
        contains('awgr-a: asked 8 times, no answer; received 0 B, sent 1 KB'),
      );
      expect(rig.tunnel.diagnosed, contains('awgr-a 198.18.0.1'));
      expect(rig.tunnels, isEmpty);
    });

    test(
      'a server that was replaced meanwhile is said to be replaced',
      () async {
        rig.probe.answers = false;
        final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw');
        final replacement = await sealedConfig(
          bodyJson(endpoint: waw.endpoint, serverByte: 31, region: 'waw'),
          seq: 50,
          at: rig.clock.now(),
        );
        var reads = 0;
        rig.gist.onFetch = () {
          if (++reads == 2) rig.gist.publish(config: replacement);
        };

        await rig.controller.connect();

        expect(rig.status.phase, Phase.failed);
        expect(rig.status.message, contains('только что заменили'));
      },
    );

    test('with the configuration unreadable it does not guess', () async {
      rig.probe.answers = false;
      var reads = 0;
      rig.gist.onFetch = () {
        if (++reads == 2) rig.gist.down = true;
      };

      await rig.controller.connect();

      expect(rig.status.phase, Phase.failed);
      expect(rig.status.message, contains('выключите другие VPN'));
      expect(rig.status.message, isNot(contains('заменили')));
      expect(rig.status.message, isNot(contains('на месте')));
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

    test('the journal says what the system said when the gist could not be '
        'read; the screen does not', () async {
      await rig.controller.connect();
      rig.gist
        ..down = true
        ..downDetail =
            'GitHub: SocketException: Failed host lookup after 10012 ms';

      await rig.controller.pollNow();

      expect(
        rig.logs,
        contains(
          'using the saved configuration: Нет связи с GitHub. '
          '(GitHub: SocketException: Failed host lookup after 10012 ms)',
        ),
      );
      expect(rig.status.message, isNot(contains('SocketException')));
    });

    test('and an honest failure without one', () async {
      final fresh = await Rig.create();
      fresh.gist
        ..down = true
        ..downDetail = 'GitHub: SocketException: Connection refused after 3 ms';
      await fresh.controller.connect();
      expect(fresh.status.phase, Phase.failed);
      expect(fresh.status.message, contains('GitHub'));
      expect(fresh.status.message, isNot(contains('SocketException')));
      expect(
        fresh.logs,
        contains(
          'the configuration could not be read: '
          '(GitHub: SocketException: Connection refused after 3 ms)',
        ),
      );
    });
  });

  group('with the files kept in two places', () {
    late FakeGist gitlab;
    late Rig two;
    late FakeServer fra;

    /// A server published only in the second place.
    Future<void> publishOnGitlab(FakeServer s, {required int seq}) async {
      gitlab.publish(
        config: await sealedConfig(
          bodyJson(
            endpoint: s.endpoint,
            serverByte: s.keyByte,
            region: s.region,
          ),
          seq: seq,
          at: two.clock.now(),
          epoch: 3,
        ),
      );
    }

    setUp(() async {
      gitlab = FakeGist();
      two = await Rig.create(
        wrap: (github) =>
            MirroredSource([github, gitlab], straggler: Duration.zero),
      );
      sto = two.fleet.add('203.0.113.9:41820', 11, region: 'sto');
      fra = two.fleet.add('203.0.113.77:41820', 12, region: 'fra');
    });

    test('works while GitHub is down, from the second place', () async {
      two.gist.down = true;
      await publishOnGitlab(sto, seq: 1);

      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(two.tunnel.confOf('awgr-a'), contains('203.0.113.9:41820'));
    });

    test('works while the second place is down, from GitHub', () async {
      await two.publish(sto);
      gitlab.down = true;

      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
    });

    test('takes the newer configuration, wherever it is', () async {
      // The second place has the move to the new server; GitHub missed it.
      await two.publish(sto, seq: 4);
      await publishOnGitlab(fra, seq: 5);
      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(two.tunnel.confOf('awgr-a'), contains('203.0.113.77:41820'));

      // And the other way round, after the app has seen both: GitHub moves on,
      // and the second place is the one that is behind now.
      await two.controller.disconnect();
      await two.publish(sto, seq: 9);
      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(two.tunnel.confOf('awgr-a'), contains('203.0.113.9:41820'));
    });

    test('a place that is behind is not written about at every look', () async {
      await two.publish(sto, seq: 9);
      await publishOnGitlab(fra, seq: 5);
      await two.controller.connect();
      await two.controller.disconnect();
      await two.controller.connect();

      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(two.tunnel.confOf('awgr-a'), contains('203.0.113.9:41820'));
      expect(
        two.logs.where((l) => l.contains('was refused')),
        isEmpty,
        reason: 'an older copy in one place is the normal case, not a problem',
      );
    });

    test('a forgery in one place is not used, and is written about', () async {
      await two.publish(sto, seq: 2);
      gitlab.publish(
        config: await sealedConfig(
          bodyJson(endpoint: '203.0.113.66:1', serverByte: 66),
          seq: 99,
          at: two.clock.now(),
          signWith: seed('root_seed'),
        ),
      );
      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(two.tunnel.confOf('awgr-a'), contains('203.0.113.9:41820'));
      expect(two.logs.where((l) => l.contains('was refused')), isNotEmpty);
    });

    test('an announcement in only one place is found', () async {
      await two.publish(sto);
      gitlab.publish(release: await sealedRelease());

      await two.controller.checkForUpdate();
      expect(two.controller.release?.build, 12);
    });

    test('a revocation in only one place is applied', () async {
      await two.publish(sto);
      await two.controller.connect();
      await two.controller.disconnect();

      gitlab.publish(revocation: await sealedRevocation(4, two.clock.now()));
      await two.controller.connect();
      expect(
        two.status.phase,
        Phase.failed,
        reason: 'the only configuration on offer is from epoch 3',
      );
    });

    test(
      'one etag is saved for both places, and it saves a request each',
      () async {
        await two.publish(sto);
        await publishOnGitlab(sto, seq: 1);
        await two.controller.connect();

        final saved = await two.store.load();
        expect(saved.etag, contains('\t'), reason: 'one part for each place');

        // Looking again asks each place with its part of the etag, and each
        // answers "unchanged".
        await two.controller.pollNow();
        await two.controller.pollNow();
        expect(two.gist.notModified, greaterThan(0));
        expect(gitlab.notModified, greaterThan(0));
      },
    );

    test('a GitHub that hangs does not hold up a connection', () async {
      await publishOnGitlab(sto, seq: 1);
      two.gist.fetchGate = Completer<void>();

      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);

      two.gist.fetchGate!.complete();
    });

    test('with both places down, the saved configuration is used', () async {
      await two.publish(sto);
      await two.controller.connect();
      await two.controller.disconnect();

      two.gist.down = true;
      gitlab.down = true;
      await two.controller.connect();
      expect(two.status.phase, Phase.connected, reason: two.status.message);
      expect(
        two.logs.any((l) => l.contains('using the saved configuration')),
        isTrue,
      );
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

    // On Android the start-up runs after the questions about permissions, and
    // the window is up behind them: a person can connect first. On the emulator
    // the answer from the settings page came six minutes late, and the start-up
    // then took the live tunnel for a leftover (2026-11-16).
    test('a connection made before the start-up ran is not cleaned up as a '
        'leftover', () async {
      await rig.controller.connect();
      rig.tunnel.log.clear();

      await rig.controller.start();

      expect(rig.status.phase, Phase.connected);
      expect(rig.tunnels, ['awgr-a']);
      expect(rig.tunnel.log, isEmpty);
      expect(rig.dns.enabledFor, '198.18.0.1');
    });

    test(
      'a failure from before the start-up ran stays on the screen',
      () async {
        sto.alive = false;
        await rig.controller.connect();
        final message = rig.status.message;

        await rig.controller.start();

        expect(rig.status.phase, Phase.failed);
        expect(rig.status.message, message);
      },
    );

    // At start-up the look for a new version runs first. On a computer whose
    // network was not yet awake it took 11 s, and the connection made with the
    // system waited all that time (2026-11 logs).
    test('a connection does not wait for a look for a new version that is '
        'still waiting on the network', () async {
      final gate = rig.gist.fetchGate = Completer<void>();
      unawaited(rig.controller.checkForUpdate());
      await pumpUntil(() => rig.gist.fetchWaiting, 'the look for a version');

      final connecting = rig.controller.connect();
      await pumpUntil(
        () => rig.logs.contains('update check: called off'),
        'the look given up for the connection',
      );
      gate.complete();
      await connecting;
      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
    });

    test('a look for a new version that has not started yet is skipped for a '
        'connection, which reads the same files', () async {
      final before = rig.gist.fetches;
      unawaited(rig.controller.checkForUpdate());
      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected);
      expect(rig.gist.fetches - before, 1);
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

      final seen = <AppStatus>[];
      final sub = rig.controller.statusStream.listen(seen.add);
      rig.tunnel.handshakeAge = const Duration(minutes: 5);
      await rig.controller.checkHealthNow();
      expect(rig.status.lostContact, isTrue);
      expect(
        seen.any((s) => s.lostContact && s.recovering),
        isTrue,
        reason: 'the way back is shown as such, not as a dead server',
      );
      expect(
        rig.gist.fetches,
        greaterThan(fetched),
        reason: 'a silent server is probably a replaced one',
      );

      rig.tunnel.handshakeAge = null;
      await rig.controller.checkHealthNow();
      for (var i = 0; i < 400; i++) {
        await Future<void>.delayed(Duration.zero);
      }
      await sub.cancel();
      expect(rig.status.lostContact, isFalse);
      expect(rig.status.recovering, isFalse);
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

  // A phone moves from Wi-Fi to mobile data and back all day. The tunnel was made
  // for the path it had, and left to its own timers it comes back after a minute
  // or never; the person's remedy was to switch it off and on.
  group('when the network changes', () {
    late StreamController<bool> net;

    // Lets the app finish whatever it is doing.
    Future<void> settle() async {
      for (var i = 0; i < 400; i++) {
        await Future<void>.delayed(Duration.zero);
      }
    }

    int rebinds() => rig.tunnel.log.where((l) => l.startsWith('rebind')).length;

    setUp(() async {
      net = StreamController<bool>.broadcast();
      rig = await Rig.create(networkChanges: net.stream);
      sto = rig.fleet.add('203.0.113.9:41820', 11, region: 'sto');
      await rig.publish(sto);
    });

    tearDown(() => net.close());

    test('the tunnel is told to find the server again, and that is enough when '
        'the server answers', () async {
      await rig.controller.connect();

      net.add(true);
      await settle();

      expect(rig.tunnel.log, contains('rebind awgr-a'));
      expect(rig.tunnels, ['awgr-a'], reason: 'no new tunnel was needed');
      expect(rig.status.phase, Phase.connected);
      expect(rig.status.lostContact, isFalse);
      expect(
        rig.logs.any((l) => l.contains('answered on the new path')),
        isTrue,
      );
    });

    test('when nothing answers on the new path the tunnel is brought up again, '
        'and the old one goes only once the new one works', () async {
      await rig.controller.connect();
      rig.tunnel.handshakeAge = const Duration(minutes: 10);
      final before = rig.tunnel.log.length;

      net.add(true);
      await settle();

      final after = rig.tunnel.log.sublist(before);
      expect(after, contains('rebind awgr-a'));
      expect(after, containsAllInOrder(['install awgr-b', 'uninstall awgr-a']));
      expect(rig.tunnels, ['awgr-b']);
      expect(rig.status.phase, Phase.connected);
      expect(rig.status.lostContact, isFalse);
    });

    test('while the connection is brought back it says so, and stops saying so '
        'once the server answers', () async {
      await rig.controller.connect();
      rig.tunnel.handshakeAge = const Duration(minutes: 10);
      final seen = <AppStatus>[];
      final sub = rig.controller.statusStream.listen(seen.add);

      net.add(true);
      await settle();
      await sub.cancel();

      expect(seen.any((s) => s.recovering), isTrue);
      expect(rig.status.phase, Phase.connected);
      expect(rig.status.recovering, isFalse);
      expect(rig.status.lostContact, isFalse);
    });

    test(
      'when bringing it back fails, it says the server does not answer',
      () async {
        await rig.controller.connect();
        rig.tunnel.handshakeAge = const Duration(minutes: 10);
        sto.alive = false;

        net.add(true);
        await settle();

        expect(rig.status.phase, Phase.connected);
        expect(rig.status.lostContact, isTrue);
        expect(rig.status.recovering, isFalse);
        expect(
          rig.logs.any((l) => l.contains('has the interface again')),
          isFalse,
          reason: 'on Windows the new tunnel never took the old one\'s adapter',
        );
      },
    );

    test('a network that is gone is shown as coming back for half a minute, '
        'then as a server that does not answer', () async {
      await rig.controller.connect();

      net.add(false);
      await settle();
      expect(rig.status.lostContact, isTrue);
      expect(rig.status.recovering, isTrue, reason: 'Wi-Fi is being found');

      rig.clock.advance(const Duration(seconds: 20));
      await rig.controller.checkHealthNow();
      expect(rig.status.recovering, isTrue, reason: 'still within the grace');

      rig.clock.advance(const Duration(seconds: 15));
      await rig.controller.checkHealthNow();
      expect(rig.status.recovering, isFalse, reason: 'past the grace');
      expect(rig.status.lostContact, isTrue);

      net.add(true);
      await settle();
      expect(rig.status.lostContact, isFalse);
      expect(rig.status.recovering, isFalse);
    });

    test('with no network at all it says so and waits', () async {
      await rig.controller.connect();

      net.add(false);
      await settle();
      expect(rig.status.lostContact, isTrue);
      expect(rig.tunnel.log, isNot(contains('rebind awgr-a')));
      expect(rig.tunnels, ['awgr-a'], reason: 'nothing to bring up against');

      net.add(true);
      await settle();
      expect(rig.tunnel.log, contains('rebind awgr-a'));
      expect(rig.status.lostContact, isFalse);
    });

    test(
      'the health check does not call a lack of network a live tunnel',
      () async {
        await rig.controller.connect();
        net.add(false);
        await settle();

        await rig.controller.checkHealthNow();
        expect(
          rig.status.lostContact,
          isTrue,
          reason: 'the handshake is fresh, but there is no network to carry it',
        );
      },
    );

    test('nothing happens when there is no connection to look after', () async {
      net.add(true);
      await settle();
      expect(rig.tunnel.log, isEmpty);
      expect(rig.status.phase, Phase.idle);
    });

    test(
      'a burst of changes is answered by where it ended, not step by step',
      () async {
        await rig.controller.connect();

        for (var i = 0; i < 6; i++) {
          net.add(i.isEven);
        }
        await settle();

        expect(
          rebinds(),
          lessThanOrEqualTo(2),
          reason: 'one pass, and one more for what arrived during it',
        );
        expect(rig.status.phase, Phase.connected);
      },
    );

    test(
      'a nudge that fails is not the end: the server is still looked for',
      () async {
        await rig.controller.connect();
        rig.tunnel.rebindError = TunnelException('boom');

        net.add(true);
        await settle();

        expect(rig.logs.any((l) => l.contains('could not be nudged')), isTrue);
        expect(rig.status.phase, Phase.connected);
        expect(rig.status.lostContact, isFalse);
      },
    );

    test('disconnecting stops the recovery', () async {
      await rig.controller.connect();
      rig.tunnel.handshakeAge = const Duration(minutes: 10);

      net.add(true);
      await rig.controller.disconnect();
      await settle();

      expect(rig.status.phase, Phase.idle);
      expect(rig.tunnels, isEmpty);
    });

    test('a handshake that cannot be read at all does not set off the repair: '
        'only an old one proves the tunnel is dead', () async {
      await rig.controller.connect();
      sto.alive = false; // the fake tunnel then has nothing to report

      await rig.controller.checkHealthNow();
      await settle();

      expect(rig.status.lostContact, isTrue);
      expect(rig.tunnels, ['awgr-a'], reason: 'nothing was brought up again');
      expect(rig.logs.any((l) => l.contains('health: no contact')), isFalse);
    });

    test(
      'a tunnel that has been silent for minutes while the network is there is '
      'brought up again without anyone asking, but not more than once in '
      'a minute and a half',
      () async {
        await rig.controller.connect();
        rig.tunnel.handshakeAge = const Duration(minutes: 5);

        await rig.controller.checkHealthNow();
        await settle();
        expect(rig.tunnels, ['awgr-b'], reason: 'brought up again');
        expect(rig.logs.any((l) => l.contains('health: no contact')), isTrue);

        await rig.controller.checkHealthNow();
        await settle();
        expect(rig.tunnels, ['awgr-b'], reason: 'too soon to try again');

        rig.clock.advance(const Duration(seconds: 100));
        await rig.controller.checkHealthNow();
        await settle();
        expect(rig.tunnels, ['awgr-a'], reason: 'the next try, in due time');
      },
    );
  });

  // What the user's main phone did all week (vivo, Android 16, 2026-11 logs): a
  // tunnel left without its interface while the screen said "connected", repairs
  // of tunnels that had just proved themselves, and a second repair right after
  // each one. The tunnel here makes handshakes the way the real library does.
  group('on a phone', () {
    late StreamController<bool> net;

    Future<void> settle() async {
      for (var i = 0; i < 400; i++) {
        await Future<void>.delayed(Duration.zero);
      }
    }

    int rebinds() => rig.tunnel.log.where((l) => l.startsWith('rebind')).length;
    int installs(String name) =>
        rig.tunnel.log.where((l) => l == 'install $name').length;

    setUp(() async {
      net = StreamController<bool>.broadcast();
      rig = await Rig.create(networkChanges: net.stream);
      sto = rig.fleet.add('203.0.113.9:41820', 11, region: 'sto');
      await rig.publish(sto);
      rig.tunnel
        ..oneAtATime = true
        ..paced = true;
      // On a phone a look at the settings goes through the tunnel.
      rig.gist.onFetch = rig.tunnel.traffic;
    });

    tearDown(() => net.close());

    test('a repair that fails gives the old tunnel its interface back, '
        'so that what the screen says is true', () async {
      await rig.controller.connect();
      rig.clock.advance(const Duration(minutes: 1));
      // The path is gone for good: no nudge, no new tunnel, no registration helps.
      sto.alive = false;

      net.add(true);
      await settle();

      expect(rig.status.lostContact, isTrue);
      expect(rig.tunnels, ['awgr-a']);
      expect(
        rig.tunnel.interfaceOwner,
        'awgr-a',
        reason:
            'the failed attempts each took the interface; the tunnel that is '
            'kept must have it back, or the phone\'s traffic goes around it',
      );

      // Once the server is back, the kept tunnel carries traffic again.
      sto.alive = true;
      expect(rig.tunnel.carries('awgr-a'), isTrue);
    });

    test('a move that fails gives the old tunnel its interface back', () async {
      await rig.controller.connect();
      final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw')
        ..alive = false;
      await rig.publish(waw);

      await rig.controller.pollNow();

      expect(rig.status.region, 'sto');
      expect(rig.status.note, contains('остаёмся на прежнем'));
      expect(rig.tunnels, ['awgr-a']);
      expect(rig.tunnel.interfaceOwner, 'awgr-a');
      expect(rig.tunnel.carries('awgr-a'), isTrue);
    });

    test('after a sleep the tunnel is nudged before anything is sent through '
        'it, nothing new is made and nothing is shown', () async {
      await rig.controller.connect();
      await rig.controller.checkHealthNow(); // the look before the phone slept
      rig.clock.advance(const Duration(minutes: 14)); // asleep: no handshakes
      rig.tunnel.log.clear();
      rig.gist.onFetch = () {
        rig.tunnel.log.add('read the settings');
        rig.tunnel.traffic();
      };
      final seen = <AppStatus>[];
      final sub = rig.controller.statusStream.listen(seen.add);

      await rig.controller.checkHealthNow();
      await settle();
      await sub.cancel();

      expect(
        rig.tunnel.log,
        ['rebind awgr-a', 'read the settings'],
        reason:
            'the first thing sent after a sleep made the handshake and got '
            'lost every tenth time on the user\'s phone: the nudge goes first, '
            'and no new tunnel is made',
      );
      expect(seen.where((s) => s.lostContact || s.recovering), isEmpty);
      expect(rig.logs, contains('wake: the server answered'));
      expect(rig.tunnel.carries('awgr-a'), isTrue);
    });

    test('on waking every timer fires at once: one look nudges, the others '
        'keep quiet, and the settings are read once, after the nudge', () async {
      await rig.controller.connect();
      await rig.controller.checkHealthNow();
      rig.clock.advance(const Duration(minutes: 14));
      rig.tunnel.log.clear();
      rig.gist.onFetch = () {
        rig.tunnel.log.add('read the settings');
        rig.tunnel.traffic();
      };
      final seen = <AppStatus>[];
      final sub = rig.controller.statusStream.listen(seen.add);

      // The poll's timer first, as on the emulator, then the health timer twice.
      await Future.wait([
        rig.controller.timedPollNow(),
        rig.controller.checkHealthNow(),
        rig.controller.checkHealthNow(),
      ]);
      await settle();
      await sub.cancel();

      expect(rig.tunnel.log, ['rebind awgr-a', 'read the settings']);
      expect(
        seen.where((s) => s.lostContact || s.recovering),
        isEmpty,
        reason: 'a second look showed "coming back" on the emulator',
      );
    });

    test('a poll whose timer fires while a look deals with the wake leaves the '
        'reading to that look', () async {
      await rig.controller.connect();
      await rig.controller.checkHealthNow();
      rig.clock.advance(const Duration(minutes: 14));
      rig.tunnel.log.clear();
      rig.gist.onFetch = () {
        rig.tunnel.log.add('read the settings');
        rig.tunnel.traffic();
      };

      // The health timer first this time, then the poll's.
      await Future.wait([
        rig.controller.checkHealthNow(),
        rig.controller.timedPollNow(),
      ]);
      await settle();

      expect(rig.tunnel.log, ['rebind awgr-a', 'read the settings']);
    });

    test('where a nudge makes no handshake (Windows), a sleep is answered by '
        'reading the settings, as before', () async {
      rig.tunnel
        ..oneAtATime = false
        ..nudgeHandshakes = false;
      await rig.controller.connect();
      await rig.controller.checkHealthNow();
      rig.clock.advance(const Duration(minutes: 14));
      rig.tunnel.log.clear();

      await rig.controller.checkHealthNow();
      await settle();

      expect(rig.tunnel.log, isEmpty, reason: 'no nudge, no new tunnel');
      expect(rig.logs, contains('health: the tunnel answered after all'));
    });

    test('a nudge after a sleep that gets no answer leaves the repair to the '
        'look that follows', () async {
      await rig.controller.connect();
      await rig.controller.checkHealthNow();
      rig.clock.advance(const Duration(minutes: 14));
      rig.tunnel.rebindError = StateError('the library is gone');

      await rig.controller.checkHealthNow();
      await settle();

      expect(
        rig.logs,
        contains(startsWith('wake: the tunnel could not be nudged')),
      );
      expect(
        rig.logs,
        contains('health: the tunnel answered after all'),
        reason: 'reading the settings still woke the tunnel',
      );
    });

    test('a change of network calls off a look at the settings that left on '
        'the old path, and is answered at once', () async {
      await rig.controller.connect();
      rig.clock.advance(const Duration(minutes: 1));
      final gate = rig.gist.fetchGate = Completer<void>();
      final poll = rig.controller.pollNow();
      await pumpUntil(() => rig.gist.fetchWaiting, 'the look at the settings');

      net.add(true);
      await settle();

      expect(
        rebinds(),
        1,
        reason:
            'the repair did not wait for the read to fail (6-32 s on the '
            'user\'s phone)',
      );
      expect(
        rig.logs,
        contains(
          'network: calling off the look at the settings, it left on the old path',
        ),
      );
      expect(
        rig.logs.any((l) => l.contains('answered on the new path')),
        isTrue,
      );
      gate.complete();
      await poll;
    });

    test('a move to a new server that has begun is not called off by a change '
        'of network', () async {
      await rig.controller.connect();
      final waw = rig.fleet.add('198.51.100.7:51820', 31, region: 'waw');
      await rig.publish(waw);
      rig.tunnel.installGate = Completer<void>();
      final poll = rig.controller.pollNow();
      await pumpUntil(() => rig.tunnel.installWaiting, 'the new tunnel');

      net.add(true);
      await Future<void>.delayed(Duration.zero);
      rig.tunnel.installGate!.complete();
      rig.tunnel.installGate = null;
      await poll;
      await settle();

      expect(rig.status.region, 'waw');
      expect(rig.logs, isNot(contains(startsWith('network: calling off'))));
    });

    test('a tunnel that really stalled is repaired once, even when two looks '
        'come in a row, and a nudge onto a new port is enough', () async {
      await rig.controller.connect();
      rig.tunnel.stuck.add('awgr-a');
      rig.clock.advance(const Duration(minutes: 4));

      await Future.wait([
        rig.controller.checkHealthNow(),
        rig.controller.checkHealthNow(),
      ]);
      await settle();

      expect(rebinds(), 1, reason: 'one repair, not one per look');
      expect(rig.tunnels, ['awgr-a'], reason: 'no new tunnel was needed');
      expect(rig.tunnel.carries('awgr-a'), isTrue);
      expect(rig.status.lostContact, isFalse);
      expect(rig.status.recovering, isFalse);
    });

    test('while one look repairs, another does not take "coming back" off the '
        'screen', () async {
      await rig.controller.connect();
      rig.tunnel.stuck.add('awgr-a');
      rig.clock.advance(const Duration(minutes: 4));
      final gate = rig.gist.fetchGate = Completer<void>();

      final first = rig.controller.checkHealthNow();
      await pumpUntil(() => rig.gist.fetchWaiting, 'the first look reads');
      final second = rig.controller.checkHealthNow();
      await Future<void>.delayed(Duration.zero);
      expect(rig.status.lostContact, isTrue);
      expect(rig.status.recovering, isTrue);

      gate.complete();
      await Future.wait([first, second]);
      await settle();
      expect(rig.status.lostContact, isFalse);
    });

    test(
      'changes that came before a new tunnel was made get no second pass',
      () async {
        // Windows-like: the nudge makes no handshake, so the tunnel is made anew.
        rig.tunnel
          ..oneAtATime = false
          ..nudgeHandshakes = false;
        await rig.controller.connect();
        rig.clock.advance(const Duration(minutes: 1));

        net.add(true);
        net.add(true); // arrives while the first is being dealt with
        await settle();

        expect(rebinds(), 1);
        expect(installs('awgr-b'), 1);
        expect(rig.tunnels, ['awgr-b']);
        expect(rig.status.lostContact, isFalse);
      },
    );

    test('a change that comes while a new tunnel is being made still gets its '
        'pass', () async {
      rig.tunnel
        ..oneAtATime = false
        ..nudgeHandshakes = false;
      await rig.controller.connect();
      rig.clock.advance(const Duration(minutes: 1));
      rig.tunnel.installGate = Completer<void>();

      net.add(true);
      await pumpUntil(() => rig.tunnel.installWaiting, 'the new tunnel');
      rig.clock.advance(const Duration(seconds: 1));
      net.add(true); // Wi-Fi gone again while the new tunnel was being made
      rig.tunnel.installGate!.complete();
      rig.tunnel.installGate = null;
      await settle();

      expect(rebinds(), 2);
    });

    test('a nudge that is answered at once shows nothing', () async {
      await rig.controller.connect();
      rig.clock.advance(const Duration(minutes: 1));
      final seen = <AppStatus>[];
      final sub = rig.controller.statusStream.listen(seen.add);

      net.add(true);
      await settle();
      await sub.cancel();

      expect(
        rig.logs.any((l) => l.contains('answered on the new path')),
        isTrue,
      );
      expect(seen.where((s) => s.recovering || s.lostContact), isEmpty);
    });
  });

  group('when the owner approves devices', () {
    late StreamController<bool> net;

    Future<void> settle() async {
      for (var i = 0; i < 400; i++) {
        await Future<void>.delayed(Duration.zero);
      }
    }

    setUp(() async {
      net = StreamController<bool>.broadcast();
      rig = await Rig.create(networkChanges: net.stream);
      sto = rig.fleet.add('203.0.113.9:41820', 11, region: 'sto');
      await rig.publish(sto);
    });

    tearDown(() => net.close());

    test('a new device waits, shows the code to match, and connects once the '
        'owner says yes', () async {
      sto.asksOwner = true;
      sto.onWaiting = (id) {
        if (sto.waiting[id]! >= 3) sto.approve(id);
      };
      final said = <String>[];
      final codes = <String>[];
      final sub = rig.controller.statusStream.listen((s) {
        said.add(s.message);
        codes.add(s.approvalCode);
      });

      await rig.controller.connect();
      await sub.cancel();

      expect(rig.status.phase, Phase.connected, reason: rig.status.message);
      expect(sto.waiting.values.single, 3, reason: 'it asked until approved');
      expect(codes.where((c) => c.isNotEmpty), isNotEmpty);
      expect(rig.status.approvalCode, isEmpty, reason: 'the wait is over');
      expect(
        said.where((m) => m.contains('Ждём одобрения владельца')),
        isNotEmpty,
      );
      final shown = said.lastWhere((m) => m.contains('Код для сверки'));
      expect(shown, matches(RegExp(r'Код для сверки: \d{4}$')));
      expect(rig.tunnels, [
        'awgr-a',
      ], reason: 'the registration tunnel is gone');
    });

    test('the code shown is the one the server shows the owner', () async {
      sto.asksOwner = true;
      sto.onWaiting = (id) {
        if (sto.waiting[id]! >= 2) sto.approve(id);
      };
      final said = <String>[];
      final sub = rig.controller.statusStream.listen(
        (s) => said.add(s.message),
      );
      await rig.controller.connect();
      await sub.cancel();

      final pub = sto.devices.keys.single;
      final code = await approvalCode(decodeStd(pub, 32));
      expect(said, contains(endsWith('Код для сверки: $code')));
    });

    test('waiting can be called off, and nothing is left behind', () async {
      sto.asksOwner = true;
      sto.onWaiting = (id) {
        if (sto.waiting[id]! >= 2) unawaited(rig.controller.cancel());
      };
      await rig.controller.connect();
      expect(rig.status.phase, Phase.idle);
      expect(rig.tunnels, isEmpty);
      expect(rig.dns.enabledFor, isNull);
    });

    test(
      'a device the owner refused is told so, and nothing is left behind',
      () async {
        sto.asksOwner = true;
        sto.onWaiting = sto.revoke;
        await rig.controller.connect();
        expect(rig.status.phase, Phase.failed);
        expect(rig.status.message, contains('закрыл доступ'));
        expect(rig.tunnels, isEmpty);
      },
    );

    test('a device cut off while connected stops and says why, instead of '
        'trying again and again', () async {
      await rig.controller.connect();
      expect(rig.status.phase, Phase.connected);
      sto.revoke(sto.devices.values.single.id);

      net.add(true);
      await settle();

      expect(rig.status.phase, Phase.failed, reason: rig.status.message);
      expect(rig.status.message, contains('закрыл доступ'));
      expect(rig.tunnels, isEmpty);
      expect(rig.dns.enabledFor, isNull);
    });

    test('a new server that wants the owner to approve this device does not '
        'take the working connection away', () async {
      await rig.controller.connect();
      final waw = rig.fleet.add('198.51.100.7:41820', 22, region: 'waw')
        ..asksOwner = true;
      await rig.publish(waw);

      await rig.controller.pollNow();

      expect(rig.status.phase, Phase.connected);
      expect(rig.status.region, 'sto', reason: 'stays on the old server');
      expect(rig.tunnels, ['awgr-a']);
      expect(waw.waiting, isNotEmpty);
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
