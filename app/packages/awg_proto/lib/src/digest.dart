import 'package:cryptography/cryptography.dart';

/// The SHA-256 of everything [chunks] delivers, as 64 lower-case hex digits: the
/// form a release announcement states it in.
///
/// It is read as it arrives, so a file of any size is checked without being held
/// in memory.
Future<String> sha256Hex(Stream<List<int>> chunks) async {
  final sink = Sha256().newHashSink();
  await for (final chunk in chunks) {
    sink.add(chunk);
  }
  sink.close();
  final hash = await sink.hash();
  final out = StringBuffer();
  for (final b in hash.bytes) {
    out.write(b.toRadixString(16).padLeft(2, '0'));
  }
  return out.toString();
}
