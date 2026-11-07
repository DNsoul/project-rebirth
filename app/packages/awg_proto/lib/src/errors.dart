/// Everything this package refuses to accept is reported as one of these. The
/// message is for a log or a developer; an app shows the user something shorter.
class ProtoException implements Exception {
  ProtoException(this.message);

  final String message;

  @override
  String toString() => 'ProtoException: $message';
}
