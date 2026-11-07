import 'errors.dart';

/// Typed reads from decoded JSON. A document with a missing or mistyped field is
/// not a document, so each of these throws instead of defaulting.
Map<String, Object?> asObject(Object? v, String what) {
  if (v is Map<String, Object?>) return v;
  if (v is Map) return v.cast<String, Object?>();
  throw ProtoException('$what is not a JSON object');
}

int intField(Map<String, Object?> j, String key) {
  final v = j[key];
  if (v is int) return v;
  throw ProtoException('field "$key" is missing or not an integer');
}

String stringField(Map<String, Object?> j, String key) {
  final v = j[key];
  if (v is String) return v;
  throw ProtoException('field "$key" is missing or not a string');
}

/// A string that may be left out, as Go's `omitempty` does.
String optionalString(Map<String, Object?> j, String key) {
  final v = j[key];
  if (v == null) return '';
  if (v is String) return v;
  throw ProtoException('field "$key" is not a string');
}

bool boolField(Map<String, Object?> j, String key) {
  final v = j[key];
  if (v is bool) return v;
  throw ProtoException('field "$key" is missing or not a boolean');
}

List<String> stringListField(Map<String, Object?> j, String key) {
  final v = j[key];
  if (v is List && v.every((e) => e is String)) return v.cast<String>();
  throw ProtoException('field "$key" is missing or not a list of strings');
}
