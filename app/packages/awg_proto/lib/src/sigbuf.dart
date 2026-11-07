import 'dart:convert';
import 'dart:typed_data';

/// Builds the exact byte string that gets signed (spec/format.md section 3).
///
/// Every field is written with an explicit length, so this and the Go side
/// always produce identical bytes without a canonical JSON encoder.
class SigBuf {
  /// Starts a buffer for the given document domain, e.g. `awg-rotator/cert/v1`.
  /// The domain plus a zero byte separates document kinds, so a signature can
  /// never be lifted from one to another.
  SigBuf(String domain) {
    _b.add(utf8.encode(domain));
    _b.addByte(0);
  }

  final BytesBuilder _b = BytesBuilder();

  void u32(int v) {
    final d = ByteData(4)..setUint32(0, v, Endian.big);
    _b.add(d.buffer.asUint8List());
  }

  void u64(int v) {
    final d = ByteData(8)..setUint64(0, v, Endian.big);
    _b.add(d.buffer.asUint8List());
  }

  void i64(int v) {
    final d = ByteData(8)..setInt64(0, v, Endian.big);
    _b.add(d.buffer.asUint8List());
  }

  /// A length-prefixed field.
  void lp(List<int> v) {
    u32(v.length);
    _b.add(v);
  }

  /// A string as a length-prefixed field of its UTF-8 bytes.
  void lps(String v) => lp(utf8.encode(v));

  Uint8List bytes() => _b.toBytes();
}
