import 'dart:convert';

import 'b64.dart';
import 'doc.dart';
import 'ed25519.dart';
import 'errors.dart';
import 'json.dart';
import 'sigbuf.dart';

/// Signing domain of a release announcement (spec/format.md section 4.6).
const domainRelease = 'awg-rotator/release/v1';

/// Platforms a release can carry an installer for.
const osWindows = 'windows';
const osAndroid = 'android';

const _maxNotes = 4000;
const _maxAssets = 4;
const _maxAssetSize = 1 << 31;

final _version = RegExp(r'^\d+\.\d+\.\d+([+-][0-9A-Za-z.-]+)?$');
final _sha256 = RegExp(r'^[0-9a-f]{64}$');

/// One installer: where to get it and what it must be.
///
/// The address is only a place to download from. What makes the file the right
/// one is the hash, which is signed with the rest, so a download server can say
/// anything it likes and the app will still refuse a file that is not this one.
class ReleaseAsset {
  const ReleaseAsset({
    required this.os,
    required this.url,
    required this.sha256,
    required this.size,
  });

  factory ReleaseAsset.fromJson(Map<String, Object?> j) => ReleaseAsset(
        os: stringField(j, 'os'),
        url: stringField(j, 'url'),
        sha256: stringField(j, 'sha256'),
        size: intField(j, 'size'),
      );

  final String os;
  final String url;

  /// Lower-case hex of the file's SHA-256.
  final String sha256;
  final int size;

  Map<String, Object?> toJson() =>
      {'os': os, 'url': url, 'sha256': sha256, 'size': size};
}

/// Says that a new version of the apps exists and where its installers are.
/// Signed by the root or backup key directly, like a command.
class Release {
  const Release({
    required this.v,
    required this.version,
    required this.build,
    required this.minBuild,
    required this.issuedAt,
    required this.notes,
    required this.assets,
    required this.issuer,
    required this.sig,
  });

  factory Release.fromJson(Map<String, Object?> j) {
    final raw = j['assets'];
    if (raw is! List) {
      throw ProtoException('field "assets" is missing or not a list');
    }
    final min = j['min_build'];
    if (min != null && min is! int) {
      throw ProtoException('field "min_build" is not an integer');
    }
    return Release(
      v: intField(j, 'v'),
      version: stringField(j, 'version'),
      build: intField(j, 'build'),
      minBuild: (min as int?) ?? 0,
      issuedAt: intField(j, 'iat'),
      notes: optionalString(j, 'notes'),
      assets: [
        for (final a in raw) ReleaseAsset.fromJson(asObject(a, 'installer')),
      ],
      issuer: stringField(j, 'by'),
      sig: stringField(j, 'sig'),
    );
  }

  final int v;

  /// For people: "1.2.0".
  final String version;

  /// For machines: grows with every release.
  final int build;

  /// The oldest build that still works against the servers. An app older than
  /// this is told that the update is not optional.
  final int minBuild;

  final int issuedAt;
  final String notes;
  final List<ReleaseAsset> assets;
  final String issuer;
  final String sig;

  /// The installer for a platform, or null if the release has none.
  ReleaseAsset? asset(String os) {
    for (final a in assets) {
      if (a.os == os) return a;
    }
    return null;
  }

  Map<String, Object?> toJson() => {
        'v': v,
        'version': version,
        'build': build,
        if (minBuild != 0) 'min_build': minBuild,
        'iat': issuedAt,
        if (notes.isNotEmpty) 'notes': notes,
        'assets': [for (final a in assets) a.toJson()],
        'by': issuer,
        'sig': sig,
      };

  /// Refuses a release that says something no app could act on.
  void check() {
    if (!_version.hasMatch(version)) {
      throw ProtoException('version "$version" is not of the form 1.2.3');
    }
    if (build < 1 || build > 0xFFFFFFFF) {
      throw ProtoException('build must be at least 1');
    }
    if (minBuild < 0 || minBuild > build) {
      throw ProtoException(
          'min_build $minBuild is above the build $build it belongs to');
    }
    if (utf8.encode(notes).length > _maxNotes) {
      throw ProtoException('the notes are too long');
    }
    if (assets.isEmpty || assets.length > _maxAssets) {
      throw ProtoException(
          'a release carries 1 to $_maxAssets installers, this one has ${assets.length}');
    }
    final seen = <String>{};
    for (final a in assets) {
      if (a.os != osWindows && a.os != osAndroid) {
        throw ProtoException('unknown platform "${a.os}"');
      }
      if (!seen.add(a.os)) throw ProtoException('two installers for ${a.os}');
      final u = Uri.tryParse(a.url);
      if (u == null || u.scheme != 'https' || u.host.isEmpty) {
        throw ProtoException(
            '${a.os}: the address must be an https URL, got "${a.url}"');
      }
      if (!_sha256.hasMatch(a.sha256)) {
        throw ProtoException(
            '${a.os}: the hash must be 64 lower-case hex digits');
      }
      if (a.size <= 0 || a.size > _maxAssetSize) {
        throw ProtoException('${a.os}: the size ${a.size} is not plausible');
      }
    }
  }

  List<int> signedBytes() {
    final s = SigBuf(domainRelease)
      ..lps(version)
      ..u32(build)
      ..u32(minBuild)
      ..i64(issuedAt)
      ..lps(notes)
      ..u32(assets.length);
    for (final a in assets) {
      s
        ..lps(a.os)
        ..lps(a.url)
        ..lps(a.sha256)
        ..u64(a.size);
    }
    s.lps(issuer);
    return s.bytes();
  }

  /// A signed copy. [seed] is the root or backup key's seed.
  Future<Release> signed(String issuer, List<int> seed) async {
    if (issuer != issuerRoot && issuer != issuerBackup) {
      throw ProtoException('unknown issuer "$issuer"');
    }
    check();
    final unsigned = Release(
      v: formatVersion,
      version: version,
      build: build,
      minBuild: minBuild,
      issuedAt: issuedAt,
      notes: notes,
      assets: assets,
      issuer: issuer,
      sig: '',
    );
    final sig = await signEd25519(seed, unsigned.signedBytes());
    return Release(
      v: formatVersion,
      version: version,
      build: build,
      minBuild: minBuild,
      issuedAt: issuedAt,
      notes: notes,
      assets: assets,
      issuer: issuer,
      sig: encode(sig),
    );
  }
}
