import 'dart:convert';
import 'dart:typed_data';

import 'errors.dart';

/// Binary values inside documents: base64url without the `=` padding.
String encode(List<int> bytes) => base64Url.encode(bytes).replaceAll('=', '');

/// Reads such a value. When [want] is not zero the length is enforced.
///
/// The check is as strict as Go's RawURLEncoding: no padding, no characters of
/// the standard alphabet. Anything else is a different encoding, and accepting
/// it would let two implementations disagree about what a document says.
Uint8List decode(String s, [int want = 0]) {
  if (!RegExp(r'^[A-Za-z0-9_-]*$').hasMatch(s)) {
    throw ProtoException('not valid base64url');
  }
  if (s.length % 4 == 1) {
    throw ProtoException('not valid base64url');
  }
  final padded = s + '=' * ((4 - s.length % 4) % 4);
  final Uint8List bytes;
  try {
    bytes = base64Url.decode(padded);
  } on FormatException {
    throw ProtoException('not valid base64url');
  }
  if (want > 0 && bytes.length != want) {
    throw ProtoException('length ${bytes.length}, want $want');
  }
  return bytes;
}

/// Keys the way a `.conf` file and the published body write them: standard
/// base64 with padding.
String encodeStd(List<int> bytes) => base64.encode(bytes);

/// Reads such a key. An empty string is "not set" and gives an empty list.
Uint8List decodeStd(String s, [int want = 0]) {
  if (s.isEmpty) return Uint8List(0);
  final Uint8List bytes;
  try {
    bytes = base64.decode(s);
  } on FormatException {
    throw ProtoException('key is not valid base64');
  }
  if (want > 0 && bytes.length != want) {
    throw ProtoException('key is ${bytes.length} bytes, want $want');
  }
  return bytes;
}
