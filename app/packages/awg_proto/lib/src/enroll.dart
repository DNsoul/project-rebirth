import 'dart:convert';
import 'dart:typed_data';

import 'b64.dart';
import 'doc.dart' show formatVersion, sealJson;
import 'envelope.dart';
import 'errors.dart';
import 'json.dart';
import 'random.dart';
import 'trust.dart' show clockSkewSeconds;

/// Device registration (spec/format.md section 4.5). Both messages are ordinary
/// envelopes with JSON inside, sent as UDP datagrams through the enrollment
/// tunnel, so nothing about them is visible from outside.

const kindEnroll = 'enroll';
const kindEnrolled = 'enrolled';

/// Sizes of the binary fields.
const deviceIdSize = 16;
const enrollNonceSize = 16;
const tunnelKeySize = 32;

/// How long the free-text label may be.
const maxDeviceName = 32;

/// Reasons an enrollment is refused.
const enrollBadRequest = 'bad_request';
const enrollBusy = 'busy';
const enrollFull = 'full';
const enrollKeyInUse = 'key_in_use';

/// Where the server answers, as seen from inside the tunnel.
const enrollServerHost = '10.8.0.1';
const enrollServerPort = 7843;

/// What a device sends.
class EnrollRequest {
  const EnrollRequest({
    required this.deviceId,
    required this.publicKey,
    required this.nonce,
    required this.issuedAt,
    this.name = '',
  });

  /// A fresh request with a random nonce. [deviceId] and [publicKey] are the raw
  /// bytes.
  factory EnrollRequest.create({
    required List<int> deviceId,
    required List<int> publicKey,
    required DateTime now,
    String name = '',
  }) {
    final request = EnrollRequest(
      deviceId: encode(deviceId),
      publicKey: encode(publicKey),
      nonce: encode(randomBytes(enrollNonceSize)),
      issuedAt: now.millisecondsSinceEpoch ~/ 1000,
      name: name,
    );
    request.check();
    return request;
  }

  factory EnrollRequest.fromJson(Map<String, Object?> j) => EnrollRequest(
        deviceId: stringField(j, 'id'),
        publicKey: stringField(j, 'pub'),
        nonce: stringField(j, 'nonce'),
        issuedAt: intField(j, 'iat'),
        name: optionalString(j, 'name'),
      )..checkVersionAndKind(j);

  /// base64url, as in the message.
  final String deviceId;
  final String publicKey;
  final String nonce;
  final int issuedAt;
  final String name;

  void checkVersionAndKind(Map<String, Object?> j) {
    if (intField(j, 'v') != formatVersion) {
      throw ProtoException(
          'enrollment format version ${intField(j, 'v')}, this build understands $formatVersion');
    }
    if (stringField(j, 'kind') != kindEnroll) {
      throw ProtoException(
          'expected an "$kindEnroll" message, got "${stringField(j, 'kind')}"');
    }
  }

  /// Throws unless every field is what the format allows.
  void check() {
    try {
      decode(deviceId, deviceIdSize);
    } on ProtoException catch (e) {
      throw ProtoException('device id: ${e.message}');
    }
    final Uint8List key;
    try {
      key = decode(publicKey, tunnelKeySize);
    } on ProtoException catch (e) {
      throw ProtoException('public key: ${e.message}');
    }
    if (key.every((b) => b == 0)) {
      throw ProtoException('public key is all zeros');
    }
    try {
      decode(nonce, enrollNonceSize);
    } on ProtoException catch (e) {
      throw ProtoException('nonce: ${e.message}');
    }
    if (name.runes.length > maxDeviceName) {
      throw ProtoException(
          'device name is longer than $maxDeviceName characters');
    }
  }

  Map<String, Object?> toJson() => {
        'v': formatVersion,
        'kind': kindEnroll,
        'id': deviceId,
        'pub': publicKey,
        'nonce': nonce,
        'iat': issuedAt,
        if (name.isNotEmpty) 'name': name,
      };

  /// The text of the datagram to send.
  Future<String> seal(List<int> contentKey) {
    check();
    return sealJson(toJson(), contentKey);
  }
}

/// Reads a request the way a server does: decrypt, check, and refuse one dated
/// too far from [now]. The app does not need this, but the tests use it to hold
/// the Dart messages to what the Go server accepts.
Future<EnrollRequest> openEnrollRequest(
    List<int> contentKey, String blob, DateTime now) async {
  final j = asObject(
      _decodeJson(await open(contentKey, blob), 'enrollment'), 'enrollment');
  final request = EnrollRequest.fromJson(j)..check();
  final age = now.millisecondsSinceEpoch ~/ 1000 - request.issuedAt;
  if (age > clockSkewSeconds || age < -clockSkewSeconds) {
    throw ProtoException(
        'enrollment is dated ${age}s from now, more than the ${clockSkewSeconds}s tolerated');
  }
  return request;
}

/// What the server answers.
class EnrollResponse {
  const EnrollResponse(
      {required this.nonce,
      required this.issuedAt,
      this.address = '',
      this.error = ''});

  factory EnrollResponse.fromJson(Map<String, Object?> j) => EnrollResponse(
        nonce: stringField(j, 'nonce'),
        issuedAt: intField(j, 'iat'),
        address: optionalString(j, 'address'),
        error: optionalString(j, 'error'),
      );

  final String nonce;
  final int issuedAt;

  /// The device's address inside the tunnel, with prefix, when it succeeded.
  final String address;

  /// One of the `enroll*` reasons when it did not.
  final String error;

  bool get granted => error.isEmpty;

  Map<String, Object?> toJson() => {
        'v': formatVersion,
        'kind': kindEnrolled,
        'nonce': nonce,
        if (address.isNotEmpty) 'address': address,
        if (error.isNotEmpty) 'error': error,
        'iat': issuedAt,
      };

  Future<String> seal(List<int> contentKey) => sealJson(toJson(), contentKey);
}

/// Decrypts a response and checks that it answers the request with [wantNonce].
Future<EnrollResponse> openEnrollResponse(
    List<int> contentKey, String blob, String wantNonce) async {
  final j =
      asObject(_decodeJson(await open(contentKey, blob), 'reply'), 'reply');
  final v = intField(j, 'v');
  final kind = stringField(j, 'kind');
  if (v != formatVersion || kind != kindEnrolled) {
    throw ProtoException('not an enrollment reply (v$v, "$kind")');
  }
  final response = EnrollResponse.fromJson(j);
  if (response.nonce != wantNonce) {
    throw ProtoException('the reply answers a different request');
  }
  if (response.error.isEmpty && response.address.isEmpty) {
    throw ProtoException(
        'the reply neither grants an address nor says why not');
  }
  return response;
}

Object? _decodeJson(Uint8List plain, String what) {
  try {
    return jsonDecode(utf8.decode(plain));
  } on FormatException {
    throw ProtoException('$what is not valid JSON');
  }
}
