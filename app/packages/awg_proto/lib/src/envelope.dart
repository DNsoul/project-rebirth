import 'dart:typed_data';

import 'package:cryptography/cryptography.dart';

import 'b64.dart';
import 'errors.dart';

/// Length of the symmetric key that encrypts everything published to the gist.
const contentKeySize = 32;

const _nonceSize = 12;
const _tagSize = 16;

final _aes = AesGcm.with256bits(nonceLength: _nonceSize);

void _checkKey(List<int> key) {
  if (key.length != contentKeySize) {
    throw ProtoException(
        'content key must be $contentKeySize bytes, got ${key.length}');
  }
}

/// Encrypts [plaintext] and returns `base64url(nonce || ciphertext || tag)`.
///
/// There is no magic header on purpose, so the text carries no recognisable
/// marker of what it is. [nonce] exists for the test vectors; real callers
/// leave it out so that every message gets a fresh one.
Future<String> seal(List<int> contentKey, List<int> plaintext,
    {List<int>? nonce}) async {
  _checkKey(contentKey);
  final n = nonce ?? _aes.newNonce();
  if (n.length != _nonceSize) {
    throw ProtoException('nonce must be $_nonceSize bytes, got ${n.length}');
  }
  final box =
      await _aes.encrypt(plaintext, secretKey: SecretKey(contentKey), nonce: n);
  return encode([...n, ...box.cipherText, ...box.mac.bytes]);
}

/// Reverses [seal]. A wrong key and a corrupted blob are the same failure as far
/// as the caller is concerned, and are reported as one.
Future<Uint8List> open(List<int> contentKey, String blob) async {
  _checkKey(contentKey);
  final Uint8List raw;
  try {
    raw = decode(blob);
  } on ProtoException catch (e) {
    throw ProtoException('blob: ${e.message}');
  }
  if (raw.length < _nonceSize + _tagSize) {
    throw ProtoException('blob is too short to be an envelope');
  }
  final box = SecretBox(
    raw.sublist(_nonceSize, raw.length - _tagSize),
    nonce: raw.sublist(0, _nonceSize),
    mac: Mac(raw.sublist(raw.length - _tagSize)),
  );
  try {
    return Uint8List.fromList(
        await _aes.decrypt(box, secretKey: SecretKey(contentKey)));
  } on SecretBoxAuthenticationError {
    throw ProtoException('blob does not decrypt with this content key');
  }
}
