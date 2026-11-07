// A small world for the controller to run in: a clock, a gist, servers that know
// devices and answer or do not, and a tunnel driver that behaves like a real one
// against them. The messages between device and server are the real ones from
// awg_proto, so what is tested is the controller's decisions, not a stand-in for
// the wire format.

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_client/core/bootstrap.dart';
import 'package:awg_client/core/gist_source.dart';
import 'package:awg_client/core/platform.dart';
import 'package:awg_proto/awg_proto.dart';

Map<String, Object?> _load(String path) =>
    jsonDecode(File(path).readAsStringSync()) as Map<String, Object?>;

final _vectors = _load('../spec/vectors.json');
Map<String, Object?> get _keys => _vectors['keys']! as Map<String, Object?>;

Uint8List seed(String name) => decode(_keys[name]! as String);

final contentKey = decode(_keys['content_key']! as String);

Uint8List filled(int byte, [int n = 32]) =>
    Uint8List.fromList(List.filled(n, byte));

class FakeClock {
  FakeClock([DateTime? start]) : _now = start ?? DateTime.utc(2026, 11, 7, 12);

  DateTime _now;

  DateTime now() => _now;

  void advance(Duration d) => _now = _now.add(d);

  Future<void> sleep(Duration d) async => advance(d);
}

Bootstrap testBootstrap() => Bootstrap(
  gistId: 'gist-1',
  rootPub: _keys['root_pub']! as String,
  backupPub: _keys['backup_pub']! as String,
  contentKey: contentKey,
);

/// The published body for a server, as the Go side writes it.
String bodyJson({
  required String endpoint,
  required int serverByte,
  String region = 'sto',
  int version = 2,
  List<String> dns = const ['198.18.0.1'],
  List<String> allowedIps = const ['198.18.0.0/15', '91.108.4.0/22'],
}) => jsonEncode({
  'version': version,
  'region': region,
  'endpoint': endpoint,
  'created_at': 1,
  'server_public_key': encodeStd(filled(serverByte)),
  'params': {
    'jc': 3,
    'jmin': 50,
    'jmax': 500,
    's1': 12,
    's2': 12,
    's3': 12,
    's4': 12,
    'header_protection_key': '',
    'random_trailers': false,
    'i1': '',
  },
  'subnet': '10.8.0.0/24',
  'dns': dns,
  'mtu': 1280,
  'allowed_ips': allowedIps,
  if (version >= 2)
    'enroll': {
      'private_key': encodeStd(filled(serverByte + 100)),
      'address': '10.8.0.251/32',
      'server': '10.8.0.1:7843',
    },
});

/// A configuration signed the way the real servers sign theirs.
Future<String> sealedConfig(
  String body, {
  required int seq,
  required DateTime at,
  int epoch = 3,
  List<int>? signWith,
}) async {
  final serverSeed = seed('server_seed');
  final cert = await Cert(
    epoch: epoch,
    notBefore: at.millisecondsSinceEpoch ~/ 1000 - 86400,
    notAfter: at.millisecondsSinceEpoch ~/ 1000 + 365 * 86400,
    serverPub: encode(await ed25519PublicFromSeed(serverSeed)),
    issuer: '',
    sig: '',
  ).signed(issuerRoot, seed('root_seed'));
  final doc = Doc(
    v: 1,
    kind: kindConfig,
    seq: seq,
    issuedAt: at.millisecondsSinceEpoch ~/ 1000,
    expiresAt: at.millisecondsSinceEpoch ~/ 1000 + 60 * 86400,
    cert: cert,
    body: body,
    sig: '',
  );
  if (signWith == null) return sealDoc(doc, serverSeed, contentKey);

  // A forgery: the library will not sign with a key the certificate does not
  // name, so the signature is made by hand over the same bytes.
  final forged = Doc(
    v: doc.v,
    kind: doc.kind,
    seq: doc.seq,
    issuedAt: doc.issuedAt,
    expiresAt: doc.expiresAt,
    cert: doc.cert,
    body: doc.body,
    sig: encode(await signEd25519(signWith, doc.signedBytes())),
  );
  return sealJson(forged.toJson(), contentKey);
}

Future<String> sealedRevocation(int minEpoch, DateTime at) async =>
    sealRevocation(
      Revocation(
        v: 1,
        minEpoch: minEpoch,
        issuedAt: at.millisecondsSinceEpoch ~/ 1000,
        issuer: '',
        sig: '',
      ),
      issuerBackup,
      seed('backup_seed'),
      contentKey,
    );

/// An announcement of a version, written the way the operator's tool writes it.
Release unsignedRelease({
  int build = 12,
  String version = '1.2.0',
  int minBuild = 0,
  String notes = 'Что нового',
  DateTime? at,
}) => Release(
  v: 1,
  version: version,
  build: build,
  minBuild: minBuild,
  issuedAt:
      (at ?? DateTime.utc(2026, 11, 7, 12)).millisecondsSinceEpoch ~/ 1000,
  notes: notes,
  assets: [
    ReleaseAsset(
      os: osWindows,
      url: 'https://example.org/Rebirth-Setup-$version.exe',
      sha256: 'ab' * 32,
      size: 1000,
    ),
    ReleaseAsset(
      os: osAndroid,
      url: 'https://example.org/Rebirth-$version.apk',
      sha256: 'cd' * 32,
      size: 2000,
    ),
  ],
  issuer: '',
  sig: '',
);

Future<String> sealedRelease({
  int build = 12,
  String version = '1.2.0',
  int minBuild = 0,
  DateTime? at,
}) async => sealRelease(
  unsignedRelease(build: build, version: version, minBuild: minBuild, at: at),
  issuerRoot,
  seed('root_seed'),
  contentKey,
);

/// An announcement claiming to come from the root key but signed by another.
Future<String> forgedRelease({int build = 99}) async {
  final r = unsignedRelease(build: build, version: '9.9.9');
  final claimed = Release(
    v: 1,
    version: r.version,
    build: r.build,
    minBuild: r.minBuild,
    issuedAt: r.issuedAt,
    notes: r.notes,
    assets: r.assets,
    issuer: issuerRoot,
    sig: '',
  );
  final sig = encode(
    await signEd25519(seed('server_seed'), claimed.signedBytes()),
  );
  return sealJson(
    Release(
      v: 1,
      version: r.version,
      build: r.build,
      minBuild: r.minBuild,
      issuedAt: r.issuedAt,
      notes: r.notes,
      assets: r.assets,
      issuer: issuerRoot,
      sig: sig,
    ).toJson(),
    contentKey,
  );
}

// -------------------------------------------------------------------- servers

class FakeServer {
  FakeServer(this.endpoint, this.keyByte, {this.region = 'sto'});

  final String endpoint;
  final int keyByte;
  final String region;
  bool alive = true;

  /// When set, registration is refused with this reason.
  String? refuse;

  /// The devices this server knows, by tunnel public key (standard base64):
  /// their identifier and the address on record.
  final Map<String, ({String id, String address})> devices = {};

  Uint8List get publicKey => filled(keyByte);
  Uint8List get enrollPrivateKey => filled(keyByte + 100);

  /// A server that took its devices over from another, as a real successor does.
  void inherit(FakeServer other) => devices.addAll(other.devices);

  String? addressOf(String pub) => devices[pub]?.address;

  Future<String?> handleEnroll(String datagram, DateTime now) async {
    if (!alive) return null;
    final EnrollRequest request;
    try {
      request = await openEnrollRequest(contentKey, datagram, now);
    } on ProtoException {
      return null;
    }
    if (refuse != null) {
      return EnrollResponse(
        nonce: request.nonce,
        issuedAt: now.millisecondsSinceEpoch ~/ 1000,
        error: refuse!,
      ).seal(contentKey);
    }
    final pubStd = encodeStd(decode(request.publicKey));
    // The same device again: the address stays, the key is replaced.
    String? address;
    devices.removeWhere((pub, d) {
      final same = d.id == request.deviceId;
      if (same) address = d.address;
      return same;
    });
    address ??= '10.8.0.${2 + devices.length}/32';
    devices[pubStd] = (id: request.deviceId, address: address!);
    return EnrollResponse(
      nonce: request.nonce,
      issuedAt: now.millisecondsSinceEpoch ~/ 1000,
      address: address!,
    ).seal(contentKey);
  }
}

class FakeFleet {
  final Map<String, FakeServer> servers = {};

  FakeServer add(String endpoint, int keyByte, {String region = 'sto'}) =>
      servers[endpoint] = FakeServer(endpoint, keyByte, region: region);

  FakeServer? byEndpoint(String endpoint) => servers[endpoint];
}

// ----------------------------------------------------------------------- gist

class FakeGist implements GistSource {
  GistSnapshot? current;
  bool down = false;
  int fetches = 0;
  int notModified = 0;
  int _etagCounter = 0;

  void publish({String? config, String? revocation, String? release}) {
    current = GistSnapshot(
      config: config ?? current?.config,
      revocation: revocation ?? current?.revocation,
      release: release ?? current?.release,
      etag: 'etag-${++_etagCounter}',
    );
  }

  /// When set, a read waits here until the test lets it go: a web request that
  /// is slow, or never answers.
  Completer<void>? fetchGate;

  /// True while a read is waiting at the gate.
  bool fetchWaiting = false;

  @override
  Future<GistSnapshot?> fetch({String? etag}) async {
    fetches++;
    final gate = fetchGate;
    if (gate != null) {
      fetchWaiting = true;
      await gate.future;
      fetchWaiting = false;
    }
    if (down) throw GistException('Нет связи с GitHub.');
    final snapshot = current;
    if (snapshot == null) throw GistException('Адрес с настройками не найден.');
    if (etag != null && etag == snapshot.etag) {
      notModified++;
      return null;
    }
    return snapshot;
  }
}

// --------------------------------------------------------------------- tunnel

class _Installed {
  _Installed(this.conf)
    : lines = {
        for (final l in conf.split('\n'))
          if (l.contains(' = '))
            l.substring(0, l.indexOf(' = ')): l.substring(l.indexOf(' = ') + 3),
      };

  final String conf;
  final Map<String, String> lines;

  String get privateKey => lines['PrivateKey']!;
  String get address => lines['Address']!;
  String get endpoint => lines['Endpoint']!;
  String get allowedIps => lines['AllowedIPs']!;
}

class FakeTunnel implements TunnelDriver {
  FakeTunnel(this.fleet, this.clock);

  final FakeFleet fleet;
  final FakeClock clock;

  /// What the driver reports as the reason it cannot work, if anything.
  String? problem;

  final Map<String, _Installed> _installed = {};
  final Map<String, String> _pubOf = {};
  final List<String> log = [];

  /// Tunnels that belong to somebody else and must never be touched.
  final Set<String> foreign = {};

  String? lastInstalled;

  /// When set, every handshake is reported as this old.
  Duration? handshakeAge;

  @override
  Future<String?> preflight() async => problem;

  /// When set, installing a tunnel waits here until the test lets it go: a way to
  /// stop the app halfway through connecting and press something.
  Completer<void>? installGate;

  /// True while an install is waiting at the gate.
  bool installWaiting = false;

  @override
  Future<void> install({required String name, required String conf}) async {
    final gate = installGate;
    if (gate != null) {
      installWaiting = true;
      await gate.future;
      installWaiting = false;
    }
    log.add('install $name');
    final t = _Installed(conf);
    _installed[name] = t;
    final pair = await tunnelKeyPairFromPrivate(decodeStd(t.privateKey, 32));
    _pubOf[name] = encodeStd(pair.publicKey);
    if (name != 'awgr-enroll') lastInstalled = name;
  }

  /// Called as a tunnel is about to be removed, so a test can look at what the
  /// screen says at the moment the slow work starts.
  void Function(String name)? onUninstall;

  @override
  Future<void> uninstall(String name) async {
    onUninstall?.call(name);
    log.add('uninstall $name');
    _installed.remove(name);
    _pubOf.remove(name);
    if (lastInstalled == name) {
      lastInstalled = _installed.keys
          .where((n) => n != 'awgr-enroll')
          .lastOrNull;
    }
  }

  @override
  Future<List<String>> installed() async => [..._installed.keys, ...foreign];

  Iterable<String> get ours => _installed.keys;

  String? confOf(String name) => _installed[name]?.conf;

  String? allowedIpsOf(String name) => _installed[name]?.allowedIps;

  FakeServer? serverOf(String name) =>
      fleet.byEndpoint(_installed[name]?.endpoint ?? '');

  @override
  Future<DateTime?> latestHandshake(String name) async {
    final t = _installed[name];
    if (t == null) return null;
    final server = fleet.byEndpoint(t.endpoint);
    if (server == null || !server.alive) return null;

    final isEnrollment =
        decodeStd(t.privateKey, 32).toString() ==
        server.enrollPrivateKey.toString();
    if (!isEnrollment && !server.devices.containsKey(_pubOf[name])) return null;
    return handshakeAge == null
        ? clock.now()
        : clock.now().subtract(handshakeAge!);
  }

  /// Whether traffic really passes through [name]: the server has to know the
  /// key and have the same address on record for it.
  bool carries(String name) {
    final t = _installed[name];
    final server = t == null ? null : fleet.byEndpoint(t.endpoint);
    if (t == null || server == null || !server.alive) return false;
    return server.addressOf(_pubOf[name]!) == t.address;
  }
}

class FakeProbe implements DataPathProbe {
  FakeProbe(this.tunnel);

  final FakeTunnel tunnel;
  final List<String> asked = [];

  /// Set to false for a tunnel that comes up but carries nothing.
  bool answers = true;

  @override
  Future<bool> resolverAnswers(String server, Duration within) async {
    asked.add(server);
    final name = tunnel.lastInstalled;
    return answers && name != null && tunnel.carries(name);
  }
}

class FakeDns implements DnsGuard {
  final List<String> log = [];
  String? enabledFor;

  @override
  Future<void> enable(String server) async {
    log.add('enable $server');
    enabledFor = server;
  }

  /// When set, taking the rule away waits here until the test lets it go; on a
  /// real machine that is the second or so PowerShell takes.
  Completer<void>? disableGate;

  @override
  Future<void> disable() async {
    log.add('disable');
    final gate = disableGate;
    if (gate != null) {
      await gate.future;
    }
    enabledFor = null;
  }
}

/// A resolver guard that cannot do its job, like one without the rights to.
class FakeDnsThatFails implements DnsGuard {
  @override
  Future<void> enable(String server) async => throw StateError('access denied');

  @override
  Future<void> disable() async => throw StateError('access denied');
}

class FakeMachine implements MachineIdSource {
  FakeMachine([this.id = 'machine-guid-1']);

  final String id;

  @override
  Future<String> machineId() async => id;
}

/// Talks to whichever server the enrollment tunnel currently points at.
class FakeTransport implements EnrollTransport {
  FakeTransport(this.tunnel, this.clock);

  final FakeTunnel tunnel;
  final FakeClock clock;
  int sent = 0;

  /// Lose this many datagrams before one gets through.
  int drop = 0;

  @override
  Future<String?> exchange(String datagram, Duration wait) async {
    sent++;
    final server = tunnel.serverOf('awgr-enroll');
    if (server == null || tunnel.allowedIpsOf('awgr-enroll') != '10.8.0.1/32') {
      clock.advance(wait);
      return null;
    }
    if (drop > 0) {
      drop--;
      clock.advance(wait);
      return null;
    }
    final reply = await server.handleEnroll(datagram, clock.now());
    if (reply == null) clock.advance(wait);
    return reply;
  }

  @override
  String describe() => 'sent $sent';

  @override
  Future<void> close() async {}
}
