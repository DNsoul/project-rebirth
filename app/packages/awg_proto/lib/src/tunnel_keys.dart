import 'dart:convert';
import 'dart:typed_data';

import 'package:cryptography/cryptography.dart';

import 'enroll.dart' show deviceIdSize, tunnelKeySize;
import 'errors.dart';
import 'random.dart';

/// A device's own tunnel key pair: Curve25519, the same as WireGuard uses.
class TunnelKeyPair {
  const TunnelKeyPair({required this.privateKey, required this.publicKey});

  /// 32 bytes, clamped the way WireGuard clamps. The private half never leaves
  /// the device.
  final Uint8List privateKey;
  final Uint8List publicKey;
}

final _x25519 = X25519();

Uint8List _clamp(List<int> raw) {
  final k = Uint8List.fromList(raw);
  k[0] &= 248;
  k[31] &= 127;
  k[31] |= 64;
  return k;
}

/// Derives the pair for a private key, so a stored private key can be turned
/// back into the public one that gets registered.
Future<TunnelKeyPair> tunnelKeyPairFromPrivate(List<int> privateKey) async {
  if (privateKey.length != tunnelKeySize) {
    throw ProtoException(
        'tunnel key must be $tunnelKeySize bytes, got ${privateKey.length}');
  }
  final clamped = _clamp(privateKey);
  final pair = await _x25519.newKeyPairFromSeed(clamped);
  final pub = await pair.extractPublicKey();
  return TunnelKeyPair(
      privateKey: clamped, publicKey: Uint8List.fromList(pub.bytes));
}

/// A new random pair.
Future<TunnelKeyPair> generateTunnelKeyPair() =>
    tunnelKeyPairFromPrivate(randomBytes(tunnelKeySize));

/// The device identifier: a hash of something unique to this installation of
/// the system (on Windows its `MachineGuid`, on Android `ANDROID_ID`) with a
/// salt that belongs to this app. The same machine always gives the same
/// identifier, so a reinstall replaces its own registration instead of adding
/// another, and the raw identifier never leaves the device.
Future<Uint8List> deviceIdFrom(String machineId,
    {String salt = 'awg-rotator/device/v1'}) async {
  if (machineId.trim().isEmpty) {
    throw ProtoException('no machine identifier to derive the device id from');
  }
  final hash = await Sha256().hash(utf8.encode('$salt\u0000$machineId'));
  return Uint8List.fromList(hash.bytes.sublist(0, deviceIdSize));
}
