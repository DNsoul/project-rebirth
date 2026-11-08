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
    this.releaseMirrors = const [],
    this.configMirrors = const [],
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
      releaseMirrors: _mirrors(j['release_mirrors'], 'release_mirrors'),
      configMirrors: _mirrors(j['config_mirrors'], 'config_mirrors'),
    );
  }

  /// The templates of the addresses of the installers' copies, checked: this is
  /// data the build was made with, so a mistake in it is a mistake of the build and
  /// is refused loudly instead of being quietly ignored.
  static List<String> _mirrors(Object? raw, String key) {
    if (raw == null) return const [];
    if (raw is! List) {
      throw ProtoException('the bundled "$key" is not a list');
    }
    final out = <String>[];
    for (final item in raw) {
      // Looked at with the braces filled in: a template has them, an address
      // does not.
      final uri = item is String
          ? Uri.tryParse(
              item.replaceAll('{file}', 'f').replaceAll('{version}', '1'),
            )
          : null;
      if (item is! String ||
          uri == null ||
          uri.scheme != 'https' ||
          uri.host.isEmpty ||
          !item.contains('{file}')) {
        throw ProtoException(
          'the bundled "$key" has an entry that is not an https '
          'address with {file} in it: "$item"',
        );
      }
      out.add(item);
    }
    return out;
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

  /// Other places the installers of a release are kept, as templates of their
  /// addresses: `{version}` is the version of the release and `{file}` the name of
  /// the installer, both from the signed announcement. An installer is only ever
  /// installed if its size and hash are the signed ones, so these places need no
  /// trust. They are here, in what the build is made with, and not in the
  /// announcement, so that they cost the announcement and the older apps nothing.
  final List<String> releaseMirrors;

  /// Other places the same signed files as in the gist are kept, as templates of
  /// their addresses with `{file}` for the name of the file (`c`, `r` or `u`).
  /// They are equals of the gist, not copies of it: the app reads all of them
  /// and takes whichever has the newest version of each file. What is in them is
  /// encrypted and signed like what is in the gist, so they need no trust.
  final List<String> configMirrors;

  final String rootPub;
  final String backupPub;
  final Uint8List contentKey;

  Trust get trust =>
      Trust(rootPub: rootPub, backupPub: backupPub, contentKey: contentKey);
}
