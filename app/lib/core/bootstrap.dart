import 'dart:convert';
import 'dart:typed_data';

import 'package:awg_proto/awg_proto.dart';

/// What an app build is made with. Holding it is what grants access: it names
/// the secret gist, carries the key that decrypts what is published there, and
/// the two public keys that anything published must be signed under.
///
/// It comes from `awgctl trust -o app/assets/bootstrap.json`.
class Bootstrap {
  Bootstrap({
    required this.gistId,
    required this.rootPub,
    required this.backupPub,
    required this.contentKey,
    this.gistOwner = '',
  });

  factory Bootstrap.fromJson(Map<String, Object?> j) {
    String field(String key) {
      final v = j[key];
      if (v is String && v.isNotEmpty) return v;
      throw ProtoException('the bundled bootstrap data has no "$key"');
    }

    return Bootstrap(
      gistId: field('gist_id'),
      gistOwner: j['gist_owner'] is String ? j['gist_owner']! as String : '',
      rootPub: field('root_pub'),
      backupPub: field('backup_pub'),
      contentKey: decode(field('content_key'), contentKeySize),
    );
  }

  factory Bootstrap.parse(String text) {
    final Object? raw;
    try {
      raw = jsonDecode(text);
    } on FormatException {
      throw ProtoException('the bundled bootstrap data is not valid JSON');
    }
    if (raw is! Map) {
      throw ProtoException('the bundled bootstrap data is not a JSON object');
    }
    return Bootstrap.fromJson(raw.cast<String, Object?>());
  }

  final String gistId;

  /// The gist's owner, which makes the raw file addresses usable as a second
  /// way to read it when the API is out of requests.
  final String gistOwner;

  final String rootPub;
  final String backupPub;
  final Uint8List contentKey;

  Trust get trust =>
      Trust(rootPub: rootPub, backupPub: backupPub, contentKey: contentKey);
}
