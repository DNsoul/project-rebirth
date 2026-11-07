import 'dart:typed_data';

import 'package:cryptography/cryptography.dart';

final _ed = Ed25519();

/// Ed25519 sizes.
const ed25519PublicKeySize = 32;
const ed25519SeedSize = 32;
const ed25519SignatureSize = 64;

Future<bool> verifyEd25519(
    List<int> publicKey, List<int> message, List<int> signature) {
  return _ed.verify(
    message,
    signature: Signature(signature,
        publicKey: SimplePublicKey(publicKey, type: KeyPairType.ed25519)),
  );
}

/// Signs with the key made from [seed]. Ed25519 is deterministic, so the same
/// seed and message always give the same signature, which is what lets the test
/// vectors pin signatures down.
Future<Uint8List> signEd25519(List<int> seed, List<int> message) async {
  final pair = await _ed.newKeyPairFromSeed(seed);
  final sig = await _ed.sign(message, keyPair: pair);
  return Uint8List.fromList(sig.bytes);
}

Future<Uint8List> ed25519PublicFromSeed(List<int> seed) async {
  final pair = await _ed.newKeyPairFromSeed(seed);
  return Uint8List.fromList((await pair.extractPublicKey()).bytes);
}
