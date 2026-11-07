import 'dart:convert';
import 'dart:typed_data';

import 'b64.dart';
import 'ed25519.dart';
import 'envelope.dart';
import 'errors.dart';
import 'json.dart';
import 'sigbuf.dart';

/// Signing domains. Each document kind has its own, so a signature made for one
/// can never be accepted as a signature for another.
const domainCert = 'awg-rotator/cert/v1';
const domainDoc = 'awg-rotator/doc/v1';
const domainRevoke = 'awg-rotator/revoke/v1';
const domainCommand = 'awg-rotator/command/v1';

/// Document kinds carried in [Doc.kind].
const kindConfig = 'config';
const kindStatus = 'status';

/// Command actions.
const actionMove = 'move';
const actionMoveRegion = 'move_region';
const actionStatus = 'status';
const actionRevoke = 'revoke';

/// Issuers allowed to sign certificates, revocations and commands.
const issuerRoot = 'root';
const issuerBackup = 'backup';

/// Written into every document and checked on the way in.
const formatVersion = 1;

/// Length of the random identifier a command carries.
const commandIdSize = 16;

void _checkIssuer(String issuer) {
  if (issuer != issuerRoot && issuer != issuerBackup) {
    throw ProtoException('unknown issuer "$issuer"');
  }
}

/// States that a server signing key may be trusted. Signed by the root or the
/// backup key.
class Cert {
  const Cert({
    required this.epoch,
    required this.notBefore,
    required this.notAfter,
    required this.serverPub,
    required this.issuer,
    required this.sig,
  });

  factory Cert.fromJson(Map<String, Object?> j) => Cert(
        epoch: intField(j, 'epoch'),
        notBefore: intField(j, 'nbf'),
        notAfter: intField(j, 'exp'),
        serverPub: stringField(j, 'pub'),
        issuer: stringField(j, 'by'),
        sig: stringField(j, 'sig'),
      );

  final int epoch;
  final int notBefore;
  final int notAfter;

  /// The server key, base64url.
  final String serverPub;
  final String issuer;
  final String sig;

  Map<String, Object?> toJson() => {
        'epoch': epoch,
        'nbf': notBefore,
        'exp': notAfter,
        'pub': serverPub,
        'by': issuer,
        'sig': sig
      };

  Uint8List signedBytes() {
    final pub = decode(serverPub, ed25519PublicKeySize);
    return (SigBuf(domainCert)
          ..u32(epoch)
          ..i64(notBefore)
          ..i64(notAfter)
          ..lp(pub)
          ..lps(issuer))
        .bytes();
  }

  /// A copy signed by [issuer], whose key is made from [seed].
  Future<Cert> signed(String issuer, List<int> seed) async {
    _checkIssuer(issuer);
    final unsigned = Cert(
        epoch: epoch,
        notBefore: notBefore,
        notAfter: notAfter,
        serverPub: serverPub,
        issuer: issuer,
        sig: '');
    final sig = await signEd25519(seed, unsigned.signedBytes());
    return Cert(
        epoch: epoch,
        notBefore: notBefore,
        notAfter: notAfter,
        serverPub: serverPub,
        issuer: issuer,
        sig: encode(sig));
  }
}

/// A configuration or a status report. Both share one shape and differ only in
/// [kind].
class Doc {
  const Doc({
    required this.v,
    required this.kind,
    required this.seq,
    required this.issuedAt,
    required this.expiresAt,
    required this.cert,
    required this.body,
    required this.sig,
  });

  factory Doc.fromJson(Map<String, Object?> j) => Doc(
        v: intField(j, 'v'),
        kind: stringField(j, 'kind'),
        seq: intField(j, 'seq'),
        issuedAt: intField(j, 'iat'),
        expiresAt: intField(j, 'exp'),
        cert: Cert.fromJson(asObject(j['cert'], 'cert')),
        body: stringField(j, 'body'),
        sig: stringField(j, 'sig'),
      );

  final int v;
  final String kind;
  final int seq;
  final int issuedAt;
  final int expiresAt;
  final Cert cert;

  /// Opaque to this package and signed exactly as stored.
  final String body;
  final String sig;

  Map<String, Object?> toJson() => {
        'v': v,
        'kind': kind,
        'seq': seq,
        'iat': issuedAt,
        'exp': expiresAt,
        'cert': cert.toJson(),
        'body': body,
        'sig': sig,
      };

  Uint8List signedBytes() {
    final pub = decode(cert.serverPub, ed25519PublicKeySize);
    return (SigBuf(domainDoc)
          ..lps(kind)
          ..u64(seq)
          ..i64(issuedAt)
          ..i64(expiresAt)
          ..lp(pub)
          ..lps(body))
        .bytes();
  }

  /// A copy signed with the server key made from [seed], which must be the one
  /// the certificate names.
  Future<Doc> signed(List<int> seed) async {
    if (kind != kindConfig && kind != kindStatus) {
      throw ProtoException('unknown document kind "$kind"');
    }
    if (encode(await ed25519PublicFromSeed(seed)) != cert.serverPub) {
      throw ProtoException(
          'signing key does not match the certificate it carries');
    }
    final unsigned = Doc(
        v: formatVersion,
        kind: kind,
        seq: seq,
        issuedAt: issuedAt,
        expiresAt: expiresAt,
        cert: cert,
        body: body,
        sig: '');
    final sig = await signEd25519(seed, unsigned.signedBytes());
    return Doc(
        v: formatVersion,
        kind: kind,
        seq: seq,
        issuedAt: issuedAt,
        expiresAt: expiresAt,
        cert: cert,
        body: body,
        sig: encode(sig));
  }
}

/// Raises the minimum acceptable certificate epoch, which is how a lost phone or
/// a seized server is cut off without rebuilding the apps.
class Revocation {
  const Revocation({
    required this.v,
    required this.minEpoch,
    required this.issuedAt,
    required this.issuer,
    required this.sig,
  });

  factory Revocation.fromJson(Map<String, Object?> j) => Revocation(
        v: intField(j, 'v'),
        minEpoch: intField(j, 'min_epoch'),
        issuedAt: intField(j, 'iat'),
        issuer: stringField(j, 'by'),
        sig: stringField(j, 'sig'),
      );

  final int v;
  final int minEpoch;
  final int issuedAt;
  final String issuer;
  final String sig;

  Map<String, Object?> toJson() => {
        'v': v,
        'min_epoch': minEpoch,
        'iat': issuedAt,
        'by': issuer,
        'sig': sig
      };

  Uint8List signedBytes() => (SigBuf(domainRevoke)
        ..u32(minEpoch)
        ..i64(issuedAt)
        ..lps(issuer))
      .bytes();

  Future<Revocation> signed(String issuer, List<int> seed) async {
    _checkIssuer(issuer);
    final unsigned = Revocation(
        v: formatVersion,
        minEpoch: minEpoch,
        issuedAt: issuedAt,
        issuer: issuer,
        sig: '');
    final sig = await signEd25519(seed, unsigned.signedBytes());
    return Revocation(
        v: formatVersion,
        minEpoch: minEpoch,
        issuedAt: issuedAt,
        issuer: issuer,
        sig: encode(sig));
  }
}

/// An instruction from the super client to the server, signed by the root or
/// backup key directly; no certificate is involved.
class Command {
  const Command({
    required this.v,
    required this.id,
    required this.issuedAt,
    required this.action,
    required this.arg,
    required this.issuer,
    required this.sig,
  });

  factory Command.fromJson(Map<String, Object?> j) => Command(
        v: intField(j, 'v'),
        id: stringField(j, 'id'),
        issuedAt: intField(j, 'iat'),
        action: stringField(j, 'action'),
        arg: optionalString(j, 'arg'),
        issuer: stringField(j, 'by'),
        sig: stringField(j, 'sig'),
      );

  final int v;
  final String id;
  final int issuedAt;
  final String action;
  final String arg;
  final String issuer;
  final String sig;

  Map<String, Object?> toJson() => {
        'v': v,
        'id': id,
        'iat': issuedAt,
        'action': action,
        if (arg.isNotEmpty) 'arg': arg,
        'by': issuer,
        'sig': sig,
      };

  Uint8List signedBytes() {
    final rawId = decode(id, commandIdSize);
    return (SigBuf(domainCommand)
          ..lp(rawId)
          ..i64(issuedAt)
          ..lps(action)
          ..lps(arg)
          ..lps(issuer))
        .bytes();
  }

  Future<Command> signed(String issuer, List<int> seed) async {
    _checkIssuer(issuer);
    if (![actionMove, actionMoveRegion, actionStatus, actionRevoke]
        .contains(action)) {
      throw ProtoException('unknown action "$action"');
    }
    final unsigned = Command(
        v: formatVersion,
        id: id,
        issuedAt: issuedAt,
        action: action,
        arg: arg,
        issuer: issuer,
        sig: '');
    final sig = await signEd25519(seed, unsigned.signedBytes());
    return Command(
        v: formatVersion,
        id: id,
        issuedAt: issuedAt,
        action: action,
        arg: arg,
        issuer: issuer,
        sig: encode(sig));
  }
}

/// Wraps any of the above for the gist.
Future<String> sealJson(Map<String, Object?> json, List<int> contentKey) =>
    seal(contentKey, utf8.encode(jsonEncode(json)));
