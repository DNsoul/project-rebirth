import 'dart:convert';
import 'dart:typed_data';

import 'b64.dart';
import 'errors.dart';
import 'json.dart';

/// The layouts of the published configuration this build reads. Version 2
/// replaced the one shared client key with the enrollment peer.
const bodyVersion = 2;
const oldestBodyVersion = 1;

/// AmneziaWG 3.1 obfuscation parameters, as published.
class AwgParams {
  const AwgParams({
    required this.jc,
    required this.jmin,
    required this.jmax,
    required this.s1,
    required this.s2,
    required this.s3,
    required this.s4,
    required this.headerProtectionKey,
    required this.randomTrailers,
    required this.i1,
  });

  factory AwgParams.fromJson(Map<String, Object?> j) => AwgParams(
        jc: intField(j, 'jc'),
        jmin: intField(j, 'jmin'),
        jmax: intField(j, 'jmax'),
        s1: intField(j, 's1'),
        s2: intField(j, 's2'),
        s3: intField(j, 's3'),
        s4: intField(j, 's4'),
        headerProtectionKey:
            decodeStd(optionalString(j, 'header_protection_key'), 32),
        randomTrailers: boolField(j, 'random_trailers'),
        i1: optionalString(j, 'i1'),
      );

  final int jc;
  final int jmin;
  final int jmax;
  final int s1;
  final int s2;
  final int s3;
  final int s4;

  /// Empty when not set.
  final Uint8List headerProtectionKey;
  final bool randomTrailers;
  final String i1;
}

/// How a device registers itself (spec/format.md section 4.5).
class EnrollInfo {
  const EnrollInfo(
      {required this.privateKey, required this.address, required this.server});

  factory EnrollInfo.fromJson(Map<String, Object?> j) => EnrollInfo(
        privateKey: decodeStd(stringField(j, 'private_key'), 32),
        address: stringField(j, 'address'),
        server: stringField(j, 'server'),
      );

  /// The enrollment peer's key. The same for every device; the server lets it
  /// reach the registration service and nothing else.
  final Uint8List privateKey;

  /// The peer's address inside the tunnel, with prefix.
  final String address;

  /// Where registration answers, as `host:port` inside the tunnel.
  final String server;
}

/// The plaintext of the configuration document.
class ConfigBody {
  const ConfigBody({
    required this.version,
    required this.region,
    required this.endpoint,
    required this.createdAt,
    required this.serverPublicKey,
    required this.params,
    required this.subnet,
    required this.dns,
    required this.mtu,
    required this.allowedIps,
    required this.enroll,
  });

  factory ConfigBody.fromJson(Map<String, Object?> j) {
    final enrollJson = j['enroll'];
    return ConfigBody(
      version: intField(j, 'version'),
      region: optionalString(j, 'region'),
      endpoint: stringField(j, 'endpoint'),
      createdAt: intField(j, 'created_at'),
      serverPublicKey: decodeStd(stringField(j, 'server_public_key'), 32),
      params: AwgParams.fromJson(asObject(j['params'], 'params')),
      subnet: stringField(j, 'subnet'),
      dns: stringListField(j, 'dns'),
      mtu: intField(j, 'mtu'),
      allowedIps: stringListField(j, 'allowed_ips'),
      enroll: enrollJson == null
          ? null
          : EnrollInfo.fromJson(asObject(enrollJson, 'enroll')),
    );
  }

  /// Reads and checks the body text carried by a verified document.
  factory ConfigBody.parse(String text) {
    final Object? raw;
    try {
      raw = jsonDecode(text);
    } on FormatException {
      throw ProtoException('configuration body is not valid JSON');
    }
    final body = ConfigBody.fromJson(asObject(raw, 'configuration body'));
    body.validate();
    return body;
  }

  final int version;
  final String region;

  /// The server's public address and port, `host:port`.
  final String endpoint;
  final int createdAt;
  final Uint8List serverPublicKey;
  final AwgParams params;
  final String subnet;
  final List<String> dns;
  final int mtu;
  final List<String> allowedIps;

  /// Null in a version 1 body, which predates device registration.
  final EnrollInfo? enroll;

  /// Throws unless a device could act on this body.
  void validate() {
    if (version < oldestBodyVersion || version > bodyVersion) {
      throw ProtoException(
          'configuration body version $version, this build understands $oldestBodyVersion to $bodyVersion');
    }
    if (version >= 2 && enroll == null) {
      throw ProtoException('no way for a device to register');
    }
    if (endpoint.isEmpty) throw ProtoException('no endpoint');
    if (serverPublicKey.length != 32 || serverPublicKey.every((b) => b == 0)) {
      throw ProtoException('no server public key');
    }
    if (allowedIps.isEmpty) throw ProtoException('no allowed addresses');
  }
}
