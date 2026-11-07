import 'dart:convert';
import 'dart:typed_data';

import 'b64.dart';
import 'doc.dart';
import 'ed25519.dart';
import 'envelope.dart';
import 'errors.dart';
import 'json.dart';
import 'release.dart';

/// How far a timestamp may sit outside its window before it is rejected, to
/// tolerate imperfect clocks.
const clockSkewSeconds = 5 * 60;

/// How old a command may be.
const commandFreshnessSeconds = 5 * 60;

/// What a verifier remembers between runs so that an old but correctly signed
/// document cannot be replayed at it.
class ProtoState {
  const ProtoState({this.minEpoch = 0, this.lastSeq = 0, this.lastBuild = 0});

  factory ProtoState.fromJson(Map<String, Object?> j) => ProtoState(
        minEpoch: intField(j, 'min_epoch'),
        lastSeq: intField(j, 'last_seq'),
        // Absent in files written before releases existed.
        lastBuild: j['last_build'] is int ? j['last_build']! as int : 0,
      );

  final int minEpoch;
  final int lastSeq;

  /// The newest release announcement seen, so that an older one cannot be
  /// replayed.
  final int lastBuild;

  ProtoState copyWith({int? minEpoch, int? lastSeq, int? lastBuild}) =>
      ProtoState(
        minEpoch: minEpoch ?? this.minEpoch,
        lastSeq: lastSeq ?? this.lastSeq,
        lastBuild: lastBuild ?? this.lastBuild,
      );

  Map<String, Object?> toJson() => {
        'min_epoch': minEpoch,
        'last_seq': lastSeq,
        if (lastBuild != 0) 'last_build': lastBuild,
      };

  @override
  bool operator ==(Object other) =>
      other is ProtoState &&
      other.minEpoch == minEpoch &&
      other.lastSeq == lastSeq &&
      other.lastBuild == lastBuild;

  @override
  int get hashCode => Object.hash(minEpoch, lastSeq, lastBuild);

  @override
  String toString() =>
      'ProtoState(minEpoch: $minEpoch, lastSeq: $lastSeq, lastBuild: $lastBuild)';
}

/// What a verifier is built with: the two keys it accepts signatures from and
/// the key that decrypts the envelope. In the apps all three are baked into the
/// build.
class Trust {
  Trust(
      {required this.rootPub,
      required this.backupPub,
      required List<int> contentKey})
      : contentKey = Uint8List.fromList(contentKey);

  /// base64url public keys.
  final String rootPub;
  final String backupPub;
  final Uint8List contentKey;

  Uint8List _issuerKey(String name) {
    final String raw;
    switch (name) {
      case issuerRoot:
        raw = rootPub;
      case issuerBackup:
        raw = backupPub;
      default:
        throw ProtoException('unknown issuer "$name"');
    }
    if (raw.isEmpty) throw ProtoException('no $name key configured');
    try {
      return decode(raw, ed25519PublicKeySize);
    } on ProtoException catch (e) {
      throw ProtoException('$name key: ${e.message}');
    }
  }

  static int _seconds(DateTime t) => t.millisecondsSinceEpoch ~/ 1000;

  static void _checkWindow(String what, int notBefore, int notAfter, int now) {
    if (notAfter <= notBefore) {
      throw ProtoException('$what validity window is empty');
    }
    if (now + clockSkewSeconds < notBefore) {
      throw ProtoException('$what is not valid yet');
    }
    if (now - clockSkewSeconds > notAfter) {
      throw ProtoException('$what has expired');
    }
  }

  /// Checks a certificate's signature, its epoch against the remembered
  /// minimum, and its validity window.
  Future<void> verifyCert(Cert c, DateTime now, ProtoState st) async {
    final key = _issuerKey(c.issuer);
    final msg = c.signedBytes();
    final Uint8List sig;
    try {
      sig = decode(c.sig, ed25519SignatureSize);
    } on ProtoException catch (e) {
      throw ProtoException('cert signature: ${e.message}');
    }
    if (!await verifyEd25519(key, msg, sig)) {
      throw ProtoException('certificate is not signed by the ${c.issuer} key');
    }
    if (c.epoch < st.minEpoch) {
      throw ProtoException(
          'certificate epoch ${c.epoch} was revoked (minimum is ${st.minEpoch})');
    }
    _checkWindow('certificate', c.notBefore, c.notAfter, _seconds(now));
  }

  Future<Map<String, Object?>> _openJson(String blob, String what) async {
    final plain = await open(contentKey, blob);
    try {
      return asObject(jsonDecode(utf8.decode(plain)), what);
    } on FormatException {
      throw ProtoException('$what is not valid JSON');
    }
  }

  /// Decrypts and fully verifies a document taken from the gist, in the order
  /// laid out in spec/format.md section 5. Returns the document and the state
  /// the caller should store. A failed call throws and returns nothing, so the
  /// caller's state cannot be changed by a document that did not pass.
  Future<(Doc, ProtoState)> openDoc(
      String blob, String wantKind, DateTime now, ProtoState st) async {
    final d = Doc.fromJson(await _openJson(blob, 'document'));
    if (d.v != formatVersion) {
      throw ProtoException(
          'document format version ${d.v}, this build understands $formatVersion');
    }
    if (d.kind != wantKind) {
      throw ProtoException('expected a "$wantKind" document, got "${d.kind}"');
    }
    await verifyCert(d.cert, now, st);

    final pub = decode(d.cert.serverPub, ed25519PublicKeySize);
    final Uint8List sig;
    try {
      sig = decode(d.sig, ed25519SignatureSize);
    } on ProtoException catch (e) {
      throw ProtoException('document signature: ${e.message}');
    }
    if (!await verifyEd25519(pub, d.signedBytes(), sig)) {
      throw ProtoException(
          'document is not signed by the key its certificate names');
    }

    // A status report is a snapshot and may legitimately arrive out of order;
    // only a configuration must never go backwards.
    if (d.kind == kindConfig && d.seq <= st.lastSeq) {
      throw ProtoException(
          'document seq ${d.seq} is not newer than the last seen ${st.lastSeq}');
    }
    _checkWindow('document', d.issuedAt, d.expiresAt, _seconds(now));

    return (d, d.kind == kindConfig ? st.copyWith(lastSeq: d.seq) : st);
  }

  /// Decrypts and verifies a revocation record. It may only raise the minimum
  /// epoch.
  Future<(Revocation, ProtoState)> openRevocation(
      String blob, DateTime now, ProtoState st) async {
    final r = Revocation.fromJson(await _openJson(blob, 'revocation'));
    if (r.v != formatVersion) {
      throw ProtoException(
          'revocation format version ${r.v}, this build understands $formatVersion');
    }
    final key = _issuerKey(r.issuer);
    final Uint8List sig;
    try {
      sig = decode(r.sig, ed25519SignatureSize);
    } on ProtoException catch (e) {
      throw ProtoException('revocation signature: ${e.message}');
    }
    if (!await verifyEd25519(key, r.signedBytes(), sig)) {
      throw ProtoException('revocation is not signed by the ${r.issuer} key');
    }
    if (r.minEpoch <= st.minEpoch) {
      throw ProtoException(
          'revocation minimum epoch ${r.minEpoch} does not raise the current ${st.minEpoch}');
    }
    return (r, st.copyWith(minEpoch: r.minEpoch));
  }

  /// Decrypts and verifies a command from the super client. Replay protection is
  /// only half done here: the caller must also reject an id it has already run.
  Future<Command> openCommand(String blob, DateTime now) async {
    final c = Command.fromJson(await _openJson(blob, 'command'));
    if (c.v != formatVersion) {
      throw ProtoException(
          'command format version ${c.v}, this build understands $formatVersion');
    }
    final key = _issuerKey(c.issuer);
    final msg = c.signedBytes();
    final Uint8List sig;
    try {
      sig = decode(c.sig, ed25519SignatureSize);
    } on ProtoException catch (e) {
      throw ProtoException('command signature: ${e.message}');
    }
    if (!await verifyEd25519(key, msg, sig)) {
      throw ProtoException('command is not signed by the ${c.issuer} key');
    }
    final age = _seconds(now) - c.issuedAt;
    if (age > commandFreshnessSeconds || age < -clockSkewSeconds) {
      throw ProtoException(
          'command timestamp is outside the accepted window (off by ${age}s)');
    }
    return c;
  }

  /// Decrypts and verifies a release announcement taken from the gist.
  ///
  /// An announcement older than one already seen is refused, so that replaying
  /// an earlier, correctly signed one cannot talk an app into an older
  /// installer. The same build again is accepted: reading the same file twice is
  /// not an attack.
  Future<(Release, ProtoState)> openRelease(String blob, ProtoState st) async {
    final r = Release.fromJson(await _openJson(blob, 'release'));
    if (r.v != formatVersion) {
      throw ProtoException(
          'release format version ${r.v}, this build understands $formatVersion');
    }
    try {
      r.check();
    } on ProtoException catch (e) {
      throw ProtoException('release: ${e.message}');
    }
    final key = _issuerKey(r.issuer);
    final Uint8List sig;
    try {
      sig = decode(r.sig, ed25519SignatureSize);
    } on ProtoException catch (e) {
      throw ProtoException('release signature: ${e.message}');
    }
    if (!await verifyEd25519(key, Uint8List.fromList(r.signedBytes()), sig)) {
      throw ProtoException('release is not signed by the ${r.issuer} key');
    }
    if (r.build < st.lastBuild) {
      throw ProtoException(
          'release build ${r.build} is older than build ${st.lastBuild} already seen');
    }
    return (r, st.copyWith(lastBuild: r.build));
  }
}

/// Signs and wraps a document with the server key for the gist.
Future<String> sealDoc(
        Doc d, List<int> serverSeed, List<int> contentKey) async =>
    sealJson((await d.signed(serverSeed)).toJson(), contentKey);

/// Signs and wraps a revocation with the root or backup key.
Future<String> sealRevocation(Revocation r, String issuer, List<int> seed,
        List<int> contentKey) async =>
    sealJson((await r.signed(issuer, seed)).toJson(), contentKey);

/// Signs and wraps a command with the root or backup key.
Future<String> sealCommand(
        Command c, String issuer, List<int> seed, List<int> contentKey) async =>
    sealJson((await c.signed(issuer, seed)).toJson(), contentKey);

/// Signs and wraps a release announcement with the root or backup key.
Future<String> sealRelease(
        Release r, String issuer, List<int> seed, List<int> contentKey) async =>
    sealJson((await r.signed(issuer, seed)).toJson(), contentKey);
