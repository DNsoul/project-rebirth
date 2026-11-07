// The release announcement (spec/format.md section 4.6), held to the vector the
// Go side wrote and to the same refusals.

import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_proto/awg_proto.dart';
import 'package:test/test.dart';

Map<String, Object?> _load(String path) =>
    jsonDecode(File(path).readAsStringSync()) as Map<String, Object?>;

final vectors = _load('../../../spec/vectors.json');
Map<String, Object?> get keys => vectors['keys']! as Map<String, Object?>;

Map<String, Object?> get releaseCase => (vectors['cases']! as List)
    .cast<Map<String, Object?>>()
    .firstWhere((c) => c['name'] == 'release');

Map<String, Object?> get releaseDoc =>
    (releaseCase['document']! as Map).cast<String, Object?>();

String hex(List<int> b) =>
    b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

Uint8List seedOf(String key) => decode(keys[key]! as String);

Uint8List get contentKey => decode(keys['content_key']! as String);

Trust trust() => Trust(
      rootPub: keys['root_pub']! as String,
      backupPub: keys['backup_pub']! as String,
      contentKey: contentKey,
    );

Matcher failsWith(String part) => throwsA(
      isA<ProtoException>().having((e) => e.message, 'message', contains(part)),
    );

Release good() => Release.fromJson({
      'v': 0,
      'version': '1.2.0',
      'build': 12,
      'min_build': 3,
      'iat': 1762400000,
      'notes': 'что нового',
      'assets': [
        {
          'os': osWindows,
          'url': 'https://example.org/Rebirth-Setup-1.2.0.exe',
          'sha256': 'ab' * 32,
          'size': 1000,
        },
        {
          'os': osAndroid,
          'url': 'https://example.org/Rebirth-1.2.0.apk',
          'sha256': 'cd' * 32,
          'size': 2000,
        },
      ],
      'by': '',
      'sig': '',
    });

/// A release with one thing changed, as the JSON a hostile gist could hold.
Release changed(void Function(Map<String, Object?> j) edit) {
  final j = good().toJson();
  edit(j);
  return Release.fromJson(j);
}

List<Map<String, Object?>> assetsOf(Map<String, Object?> j) =>
    (j['assets']! as List).cast<Map<String, Object?>>();

void main() {
  group('the vector from the Go side', () {
    test('the bytes that are signed, and the signature, are the same',
        () async {
      final r = Release.fromJson(releaseDoc);
      expect(hex(r.signedBytes()), releaseCase['signed_bytes_hex']);
      expect(r.sig, releaseCase['sig']);
      expect((await r.signed(issuerRoot, seedOf('root_seed'))).sig,
          releaseCase['sig']);
    });

    test('it opens, and says what it says', () async {
      final blob = await sealJson(releaseDoc, contentKey);
      final (r, next) = await trust().openRelease(blob, const ProtoState());
      expect(r.version, '1.2.0');
      expect(r.build, 12);
      expect(r.minBuild, 3);
      expect(r.asset(osWindows)!.size, 28311552);
      expect(r.asset(osAndroid)!.url, startsWith('https://'));
      expect(r.notes, 'Новая версия: быстрее подключается.');
      expect(next.lastBuild, 12);
    });
  });

  group('a release built here', () {
    test('is read back, from the root key or the backup key', () async {
      for (final (issuer, seed) in [
        (issuerRoot, seedOf('root_seed')),
        (issuerBackup, seedOf('backup_seed')),
      ]) {
        final blob = await sealRelease(good(), issuer, seed, contentKey);
        final (r, _) = await trust().openRelease(blob, const ProtoState());
        expect(r.issuer, issuer);
        expect(r.assets, hasLength(2));
        expect(r.asset('ios'), isNull);
      }
    });

    test('is signed by one of the two keys and nobody else', () async {
      await expectLater(
          good().signed('nobody', seedOf('root_seed')), failsWith('issuer'));
    });

    test('is not accepted if signed with the wrong key', () async {
      // Signed with the backup seed but claiming to be from root.
      final liar = await good().signed(issuerBackup, seedOf('backup_seed'));
      final j = liar.toJson()..['by'] = issuerRoot;
      final blob = await sealJson(j, contentKey);
      await expectLater(trust().openRelease(blob, const ProtoState()),
          failsWith('not signed by the root key'));
    });

    test('has every field signed', () async {
      final base =
          (await good().signed(issuerRoot, seedOf('root_seed'))).toJson();
      final edits = <String, void Function(Map<String, Object?>)>{
        'the version': (j) => j['version'] = '9.9.9',
        'the build': (j) => j['build'] = 99,
        'the minimum': (j) => j['min_build'] = 11,
        'the date': (j) => j['iat'] = 1762400001,
        'the notes': (j) => j['notes'] = 'другое',
        'an address': (j) =>
            assetsOf(j)[0]['url'] = 'https://evil.example/x.exe',
        'a hash': (j) => assetsOf(j)[1]['sha256'] = 'ef' * 32,
        'a size': (j) => assetsOf(j)[0]['size'] = 1001,
        'the issuer': (j) => j['by'] = issuerBackup,
        'the number of installers': (j) => j['assets'] = [assetsOf(j)[0]],
      };
      for (final e in edits.entries) {
        final j = jsonDecode(jsonEncode(base)) as Map<String, Object?>;
        e.value(j);
        final blob = await sealJson(j, contentKey);
        await expectLater(
          trust().openRelease(blob, const ProtoState()),
          throwsA(isA<ProtoException>()),
          reason: 'changing ${e.key} after signing was not noticed',
        );
      }
    });
  });

  group('what a release may say', () {
    final nonsense = <String, void Function(Map<String, Object?>)>{
      'a version with letters': (j) => j['version'] = 'latest',
      'no build': (j) => j['build'] = 0,
      'a minimum above the build': (j) => j['min_build'] = 13,
      'no installers': (j) => j['assets'] = <Object?>[],
      'two for one platform': (j) => assetsOf(j)[1]['os'] = osWindows,
      'an unknown platform': (j) => assetsOf(j)[0]['os'] = 'plan9',
      'a plain http address': (j) =>
          assetsOf(j)[0]['url'] = 'http://example.org/a.exe',
      'a file address': (j) => assetsOf(j)[0]['url'] = 'file:///c:/a.exe',
      'an address with no host': (j) =>
          assetsOf(j)[0]['url'] = 'https:///a.exe',
      'a hash in capitals': (j) => assetsOf(j)[0]['sha256'] = 'AB' * 32,
      'a short hash': (j) => assetsOf(j)[0]['sha256'] = 'abcd',
      'an empty file': (j) => assetsOf(j)[0]['size'] = 0,
      'notes that are too long': (j) => j['notes'] = 'я' * 4000,
    };
    for (final e in nonsense.entries) {
      test('${e.key} is refused at signing', () async {
        await expectLater(
          changed(e.value).signed(issuerRoot, seedOf('root_seed')),
          throwsA(isA<ProtoException>()),
        );
      });
    }
  });

  group('replay and mix-ups', () {
    test('an older announcement is refused once a newer one was seen',
        () async {
      final old = changed((j) {
        j['build'] = 11;
        j['version'] = '1.1.0';
      });
      final blob =
          await sealRelease(old, issuerRoot, seedOf('root_seed'), contentKey);
      await expectLater(
          trust().openRelease(blob, const ProtoState(lastBuild: 12)),
          failsWith('older than build 12'));
    });

    test('the same build twice is only reading the file twice', () async {
      final blob = await sealRelease(
          good(), issuerRoot, seedOf('root_seed'), contentKey);
      final (_, next) =
          await trust().openRelease(blob, const ProtoState(lastBuild: 12));
      expect(next.lastBuild, 12);
    });

    test('a release is not a command, and a command is not a release',
        () async {
      final blob = await sealRelease(
          good(), issuerRoot, seedOf('root_seed'), contentKey);
      await expectLater(
          trust().openCommand(
              blob,
              DateTime.fromMillisecondsSinceEpoch(1762400060 * 1000,
                  isUtc: true)),
          throwsA(isA<ProtoException>()));
      final command = await sealJson(
          (vectors['cases']! as List)
                  .cast<Map<String, Object?>>()
                  .firstWhere((c) => c['name'] == 'command')['document']!
              as Map<String, Object?>,
          contentKey);
      await expectLater(trust().openRelease(command, const ProtoState()),
          throwsA(isA<ProtoException>()));
    });

    test('another content key never opens it', () async {
      final blob = await sealRelease(
          good(), issuerRoot, seedOf('root_seed'), contentKey);
      final other = Trust(
          rootPub: keys['root_pub']! as String,
          backupPub: keys['backup_pub']! as String,
          contentKey: Uint8List(32)..fillRange(0, 32, 0x44));
      await expectLater(other.openRelease(blob, const ProtoState()),
          throwsA(isA<ProtoException>()));
    });
  });

  group('the remembered state', () {
    test('a file written before releases existed still reads', () {
      final s = ProtoState.fromJson({'min_epoch': 2, 'last_seq': 9});
      expect(s.lastBuild, 0);
      expect(s.toJson(), {'min_epoch': 2, 'last_seq': 9},
          reason: 'nothing new is written until there is something to say');
    });

    test('the newest build is kept next to the rest', () {
      const s = ProtoState(minEpoch: 2, lastSeq: 9, lastBuild: 12);
      expect(ProtoState.fromJson(s.toJson()), s);
      expect(s.copyWith(lastSeq: 10).lastBuild, 12);
    });
  });
}
