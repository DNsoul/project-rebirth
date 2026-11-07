import 'dart:math';
import 'dart:typed_data';

final _random = Random.secure();

/// [n] bytes from the operating system's secure random source: nonces, device
/// keys. Nothing here may use a predictable generator.
Uint8List randomBytes(int n) =>
    Uint8List.fromList(List<int>.generate(n, (_) => _random.nextInt(256)));
