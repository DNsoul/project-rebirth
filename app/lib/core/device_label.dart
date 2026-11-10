import 'package:awg_proto/awg_proto.dart' show maxDeviceName;

/// The name this device goes by in the owner's chat: what the phone calls its
/// model, or the computer's name. It is only for the owner's eyes, but it is
/// all they have to tell one request from another next to the code, so it is
/// kept readable: one line, no control characters, and short enough for the
/// server to take. [fallback] stands in when there is nothing usable.
String deviceLabel(String raw, {required String fallback}) {
  final cleaned = raw
      .replaceAll(RegExp(r'[\x00-\x1f\x7f]'), ' ')
      .replaceAll(RegExp(r'\s+'), ' ')
      .trim();
  final name = cleaned.isEmpty ? fallback : cleaned;
  final runes = name.runes.toList();
  if (runes.length <= maxDeviceName) {
    return name;
  }
  return String.fromCharCodes(runes.take(maxDeviceName)).trimRight();
}
