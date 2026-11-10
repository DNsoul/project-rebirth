// These tests hold the Dart port to the vectors the Go implementation wrote
// (spec/vectors.json and spec/conf-vectors.json). If a byte differs, the two
// disagree about what a document says, and one of them is wrong.

import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:awg_proto/awg_proto.dart';
import 'package:test/test.dart';

Map<String, Object?> _load(String path) =>
    jsonDecode(File(path).readAsStringSync()) as Map<String, Object?>;

final vectors = _load('../../../spec/vectors.json');
final confVectors = _load('../../../spec/conf-vectors.json');

Map<String, Object?> get keys => vectors['keys']! as Map<String, Object?>;
List<Map<String, Object?>> get cases =>
    (vectors['cases']! as List).cast<Map<String, Object?>>();
Map<String, Object?> get envelopeVec =>
    vectors['envelope']! as Map<String, Object?>;
Map<String, Object?> get enrollVec =>
    vectors['enroll']! as Map<String, Object?>;

Map<String, Object?> caseDoc(String name) =>
    (cases.firstWhere((c) => c['name'] == name)['document']! as Map)
        .cast<String, Object?>();

Map<String, Object?> caseNamed(String name) =>
    cases.firstWhere((c) => c['name'] == name);

String hex(List<int> b) =>
    b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

Uint8List seedOf(String key) => decode(keys[key]! as String);

/// Seconds the vectors were made at.
const iat = 1762400000;

DateTime at(int seconds) =>
    DateTime.fromMillisecondsSinceEpoch(seconds * 1000, isUtc: true);

Trust trust({List<int>? contentKey}) => Trust(
      rootPub: keys['root_pub']! as String,
      backupPub: keys['backup_pub']! as String,
      contentKey: contentKey ?? decode(keys['content_key']! as String),
    );

Matcher failsWith(String part) => throwsA(
      isA<ProtoException>().having((e) => e.message, 'message', contains(part)),
    );

void main() {
  group('keys and the envelope', () {
    test('every seed gives the published public key', () async {
      // The Go side wrote the public keys; if the seed expansion differed, no
      // signature below could match either.
      expect(encode(await ed25519PublicFromSeed(seedOf('root_seed'))),
          keys['root_pub']);
      expect(encode(await ed25519PublicFromSeed(seedOf('backup_seed'))),
          keys['backup_pub']);
      expect(encode(await ed25519PublicFromSeed(seedOf('server_seed'))),
          keys['server_pub']);
    });

    test('opens the blob Go sealed', () async {
      final plain = await open(decode(keys['content_key']! as String),
          envelopeVec['blob']! as String);
      expect(plain, decode(envelopeVec['plaintext_b64']! as String));
    });

    test('seals to exactly the same blob when given the same nonce', () async {
      final blob = await seal(
        decode(keys['content_key']! as String),
        decode(envelopeVec['plaintext_b64']! as String),
        nonce: decode(envelopeVec['nonce']! as String),
      );
      expect(blob, envelopeVec['blob']);
    });

    test('a wrong key and a damaged blob fail the same way', () async {
      final blob = envelopeVec['blob']! as String;
      await expectLater(
          open(Uint8List(32), blob), failsWith('does not decrypt'));
      final damaged = blob.substring(0, blob.length - 4) +
          (blob.endsWith('AAAA') ? 'BBBB' : 'AAAA');
      await expectLater(open(decode(keys['content_key']! as String), damaged),
          failsWith('does not decrypt'));
      await expectLater(open(decode(keys['content_key']! as String), 'AAAA'),
          failsWith('too short'));
      await expectLater(
          open(decode(keys['content_key']! as String), 'not base64!'),
          failsWith('base64url'));
    });

    test('nonces are fresh', () async {
      final key = Uint8List(32);
      expect(await seal(key, [1, 2, 3]), isNot(await seal(key, [1, 2, 3])));
    });

    test('base64url is strict', () {
      expect(() => decode('ab=='), throwsA(isA<ProtoException>()));
      expect(() => decode('a+b/'), throwsA(isA<ProtoException>()));
      expect(() => decode('a'), throwsA(isA<ProtoException>()));
      expect(decode('AQID'), [1, 2, 3]);
      expect(encode([1, 2, 3, 4]), 'AQIDBA');
    });
  });

  group('signed bytes and signatures', () {
    test('cert', () async {
      final c = caseNamed('cert');
      final cert = Cert.fromJson(caseDoc('cert'));
      expect(hex(cert.signedBytes()), c['signed_bytes_hex']);
      expect(cert.sig, c['sig']);
      expect(
          (await cert.signed(issuerRoot, seedOf('root_seed'))).sig, c['sig']);
    });

    test('config', () async {
      final c = caseNamed('config');
      final doc = Doc.fromJson(caseDoc('config'));
      expect(hex(doc.signedBytes()), c['signed_bytes_hex']);
      expect(doc.sig, c['sig']);
      expect((await doc.signed(seedOf('server_seed'))).sig, c['sig']);
    });

    test('revocation', () async {
      final c = caseNamed('revocation');
      final r = Revocation.fromJson(caseDoc('revocation'));
      expect(hex(r.signedBytes()), c['signed_bytes_hex']);
      expect(r.sig, c['sig']);
      expect(
          (await r.signed(issuerBackup, seedOf('backup_seed'))).sig, c['sig']);
    });

    test('command', () async {
      final c = caseNamed('command');
      final cmd = Command.fromJson(caseDoc('command'));
      expect(hex(cmd.signedBytes()), c['signed_bytes_hex']);
      expect(cmd.sig, c['sig']);
      expect((await cmd.signed(issuerRoot, seedOf('root_seed'))).sig, c['sig']);
    });

    test('a document will not be signed by a key its certificate does not name',
        () async {
      final doc = Doc.fromJson(caseDoc('config'));
      await expectLater(doc.signed(seedOf('root_seed')),
          failsWith('does not match the certificate'));
    });

    test('an unknown issuer, kind or action is refused', () async {
      await expectLater(
          Cert.fromJson(caseDoc('cert')).signed('nobody', seedOf('root_seed')),
          failsWith('issuer'));
      await expectLater(
          Command.fromJson(caseDoc('command'))
              .signed('nobody', seedOf('root_seed')),
          failsWith('issuer'));
      final bad =
          Command.fromJson({...caseDoc('command'), 'action': 'format_disk'});
      await expectLater(
          bad.signed(issuerRoot, seedOf('root_seed')), failsWith('action'));
    });
  });

  group('opening a configuration, in the order of spec section 5', () {
    late String blob;
    final now = at(iat + 3600);

    setUp(() async {
      blob = await sealJson(
          caseDoc('config'), decode(keys['content_key']! as String));
    });

    test('a good one opens and moves the remembered seq forward', () async {
      final (doc, state) =
          await trust().openDoc(blob, kindConfig, now, const ProtoState());
      expect(doc.seq, 7);
      expect(doc.cert.epoch, 3);
      expect(state, const ProtoState(lastSeq: 7));
    });

    test('an old seq is refused', () async {
      await expectLater(
        trust().openDoc(blob, kindConfig, now, const ProtoState(lastSeq: 7)),
        failsWith('not newer'),
      );
    });

    test('a revoked epoch is refused', () async {
      await expectLater(
        trust().openDoc(blob, kindConfig, now, const ProtoState(minEpoch: 4)),
        failsWith('revoked'),
      );
    });

    test('the wrong content key is refused', () async {
      await expectLater(
        trust(contentKey: Uint8List(32))
            .openDoc(blob, kindConfig, now, const ProtoState()),
        failsWith('does not decrypt'),
      );
    });

    test('the wrong kind is refused', () async {
      await expectLater(
          trust().openDoc(blob, kindStatus, now, const ProtoState()),
          failsWith('expected a "status"'));
    });

    test('a tampered body is refused', () async {
      final tampered = {
        ...caseDoc('config'),
        'body': '{"endpoint":"203.0.113.66:51820"}'
      };
      final bad =
          await sealJson(tampered, decode(keys['content_key']! as String));
      await expectLater(
        trust().openDoc(bad, kindConfig, now, const ProtoState()),
        failsWith('not signed by the key its certificate names'),
      );
    });

    test('a certificate that was not issued by a trusted key is refused',
        () async {
      final doc = caseDoc('config');
      final cert = {
        ...(doc['cert']! as Map).cast<String, Object?>(),
        'by': 'backup'
      };
      final bad = await sealJson(
          {...doc, 'cert': cert}, decode(keys['content_key']! as String));
      await expectLater(
        trust().openDoc(bad, kindConfig, now, const ProtoState()),
        failsWith('not signed by the backup key'),
      );
    });

    test('a server key nobody certified is refused', () async {
      final doc = caseDoc('config');
      final cert = {
        ...(doc['cert']! as Map).cast<String, Object?>(),
        'pub': encode(await ed25519PublicFromSeed(Uint8List(32))),
      };
      final bad = await sealJson(
          {...doc, 'cert': cert}, decode(keys['content_key']! as String));
      await expectLater(
          trust().openDoc(bad, kindConfig, now, const ProtoState()),
          failsWith('not signed by the root key'));
    });

    test('too early and too late are refused, with the clock tolerance',
        () async {
      await expectLater(
        trust().openDoc(blob, kindConfig, at(iat - 3600), const ProtoState()),
        failsWith('not valid yet'),
      );
      await expectLater(
        trust().openDoc(
            blob, kindConfig, at(iat + 61 * 24 * 3600), const ProtoState()),
        failsWith('expired'),
      );
      // Within five minutes of the edge is let through.
      await trust().openDoc(blob, kindConfig, at(iat - clockSkewSeconds + 30),
          const ProtoState());
    });

    test('garbage is not a document', () async {
      final key = decode(keys['content_key']! as String);
      await expectLater(
        trust().openDoc(await seal(key, utf8.encode('not json')), kindConfig,
            now, const ProtoState()),
        failsWith('not valid JSON'),
      );
      await expectLater(
        trust().openDoc(await seal(key, utf8.encode('{"v":1}')), kindConfig,
            now, const ProtoState()),
        throwsA(isA<ProtoException>()),
      );
    });

    test('a status report may repeat or go back in seq', () async {
      final status =
          await Doc.fromJson({...caseDoc('config'), 'kind': 'status', 'seq': 1})
              .signed(seedOf('server_seed'));
      final statusBlob = await sealJson(
          status.toJson(), decode(keys['content_key']! as String));
      final (_, state) = await trust()
          .openDoc(statusBlob, kindStatus, now, const ProtoState(lastSeq: 50));
      expect(state, const ProtoState(lastSeq: 50),
          reason: 'a status never moves the config seq');
    });

    test('a document built here is read back by the same code', () async {
      final key = decode(keys['content_key']! as String);
      final cert = Cert.fromJson(caseDoc('cert'));
      final doc = Doc(
          v: 1,
          kind: kindConfig,
          seq: 99,
          issuedAt: iat,
          expiresAt: iat + 86400,
          cert: cert,
          body: 'сайт ✓ {"a":1}',
          sig: '');
      final sealed = await sealDoc(doc, seedOf('server_seed'), key);
      final (opened, _) =
          await trust().openDoc(sealed, kindConfig, now, const ProtoState());
      expect(opened.body, 'сайт ✓ {"a":1}',
          reason: 'the body is signed byte for byte, whatever it holds');
    });
  });

  group('revocation and commands', () {
    final key = decode(keys['content_key']! as String);

    test('the revocation in the vectors raises the minimum epoch', () async {
      final blob = await sealJson(caseDoc('revocation'), key);
      final (r, state) = await trust().openRevocation(
          blob, at(iat), const ProtoState(minEpoch: 3, lastSeq: 9));
      expect(r.minEpoch, 4);
      expect(state, const ProtoState(minEpoch: 4, lastSeq: 9));
    });

    test('a revocation that does not raise the epoch is refused', () async {
      final blob = await sealJson(caseDoc('revocation'), key);
      await expectLater(
        trust().openRevocation(blob, at(iat), const ProtoState(minEpoch: 4)),
        failsWith('does not raise'),
      );
    });

    test('a tampered revocation is refused', () async {
      final blob =
          await sealJson({...caseDoc('revocation'), 'min_epoch': 9}, key);
      await expectLater(
        trust().openRevocation(blob, at(iat), const ProtoState()),
        failsWith('not signed by the backup key'),
      );
    });

    test('the command in the vectors opens while it is fresh', () async {
      final blob = await sealJson(caseDoc('command'), key);
      final cmd = await trust().openCommand(blob, at(iat + 60));
      expect(cmd.action, actionMoveRegion);
      expect(cmd.arg, 'waw');
    });

    test('a stale or future command is refused', () async {
      final blob = await sealJson(caseDoc('command'), key);
      await expectLater(trust().openCommand(blob, at(iat + 10 * 60)),
          failsWith('outside the accepted window'));
      await expectLater(trust().openCommand(blob, at(iat - 10 * 60)),
          failsWith('outside the accepted window'));
    });

    test('a command from an untrusted key is refused', () async {
      final other = Trust(
          rootPub: encode(await ed25519PublicFromSeed(Uint8List(32))),
          backupPub: keys['backup_pub']! as String,
          contentKey: key);
      final blob = await sealJson(caseDoc('command'), key);
      await expectLater(other.openCommand(blob, at(iat + 60)),
          failsWith('not signed by the root key'));
    });

    test('a command built here is read back', () async {
      final id = encode(List.generate(16, (i) => i));
      final cmd = Command(
          v: 1,
          id: id,
          issuedAt: iat,
          action: actionMove,
          arg: '',
          issuer: '',
          sig: '');
      final blob = await sealCommand(cmd, issuerRoot, seedOf('root_seed'), key);
      final opened = await trust().openCommand(blob, at(iat + 5));
      expect(opened.action, actionMove);
      expect(opened.arg, isEmpty);
    });
  });

  group('device registration', () {
    final key = decode(keys['content_key']! as String);
    final e = enrollVec;
    final now = at(e['now']! as int);

    test('reads the request Go sealed', () async {
      final r = await openEnrollRequest(key, e['request_blob']! as String, now);
      expect(r.deviceId, e['device_id']);
      expect(r.publicKey, e['public_key']);
      expect(r.nonce, e['request_nonce']);
      expect(r.name, e['name']);
    });

    test('reads the reply and the refusal Go sealed', () async {
      final reply = await openEnrollResponse(
          key, e['reply_blob']! as String, e['request_nonce']! as String);
      expect(reply.granted, isTrue);
      expect(reply.address, e['address']);

      final refused = await openEnrollResponse(
          key, e['refused_blob']! as String, e['request_nonce']! as String);
      expect(refused.granted, isFalse);
      expect(refused.error, enrollFull);
    });

    test('writes the same JSON Go does, field for field and in the same order',
        () {
      final request = EnrollRequest(
        deviceId: e['device_id']! as String,
        publicKey: e['public_key']! as String,
        nonce: e['request_nonce']! as String,
        issuedAt: e['now']! as int,
        name: e['name']! as String,
      );
      expect(jsonEncode(request.toJson()), e['request_plain']);

      final reply = EnrollResponse(
          nonce: e['request_nonce']! as String,
          issuedAt: e['now']! as int,
          address: e['address']! as String);
      expect(jsonEncode(reply.toJson()), e['reply_plain']);

      final refused = EnrollResponse(
          nonce: e['request_nonce']! as String,
          issuedAt: e['now']! as int,
          error: enrollFull);
      expect(jsonEncode(refused.toJson()), e['refused_plain']);
    });

    test('derives the same approval code Go does', () async {
      expect(await approvalCode(decode(e['public_key']! as String)),
          e['approval_code']);
    });

    test('approval codes are always four digits', () async {
      for (var i = 0; i < 64; i++) {
        final code = await approvalCode(List.filled(32, i));
        expect(code, matches(RegExp(r'^\d{4}$')));
      }
    });

    test('a reply to another request is refused', () async {
      await expectLater(
        openEnrollResponse(
            key, e['reply_blob']! as String, encode(Uint8List(16))),
        failsWith('different request'),
      );
    });

    test('a request made here is read back, and is fresh each time', () async {
      final device = Uint8List.fromList(List.generate(16, (i) => i + 1));
      final pub = Uint8List.fromList(List.generate(32, (i) => i + 9));
      final a = EnrollRequest.create(
          deviceId: device, publicKey: pub, now: now, name: 'windows');
      final b = EnrollRequest.create(
          deviceId: device, publicKey: pub, now: now, name: 'windows');
      expect(a.nonce, isNot(b.nonce));

      final back = await openEnrollRequest(key, await a.seal(key), now);
      expect(back.deviceId, encode(device));
      expect(back.publicKey, encode(pub));
      expect(back.name, 'windows');
    });

    test('a request outside the clock tolerance is refused', () async {
      final r = EnrollRequest.create(
          deviceId: Uint8List(16)..[0] = 1,
          publicKey: Uint8List(32)..[0] = 1,
          now: now);
      final blob = await r.seal(key);
      await expectLater(
          openEnrollRequest(key, blob, at((e['now']! as int) + 3600)),
          failsWith('tolerated'));
    });

    test('malformed requests are not created', () {
      final id = Uint8List(16)..[0] = 1;
      final pub = Uint8List(32)..[0] = 1;
      expect(
          () => EnrollRequest.create(
              deviceId: Uint8List(8), publicKey: pub, now: now),
          failsWith('device id'));
      expect(
          () => EnrollRequest.create(
              deviceId: id, publicKey: Uint8List(31), now: now),
          failsWith('public key'));
      expect(
          () => EnrollRequest.create(
              deviceId: id, publicKey: Uint8List(32), now: now),
          failsWith('zeros'));
      expect(
          () => EnrollRequest.create(
              deviceId: id, publicKey: pub, now: now, name: 'я' * 33),
          failsWith('longer'));
    });

    test('the device id is stable per machine and never the raw identifier',
        () async {
      final a = await deviceIdFrom('machine-guid-1');
      expect(a.length, 16);
      expect(await deviceIdFrom('machine-guid-1'), a);
      expect(await deviceIdFrom('machine-guid-2'), isNot(a));
      expect(
          await deviceIdFrom('machine-guid-1', salt: 'another app'), isNot(a));
      expect(utf8.decode(a, allowMalformed: true), isNot(contains('machine')));
      await expectLater(deviceIdFrom('  '), failsWith('machine identifier'));
    });
  });

  group('the client configuration file', () {
    final cases = (confVectors['cases']! as List).cast<Map<String, Object?>>();

    for (final c in cases) {
      test(c['name']! as String, () async {
        final body = ConfigBody.parse(c['body']! as String);
        final private = decodeStd(c['private_key']! as String, 32);

        // What the file says is what Go wrote, to the byte.
        final conf = renderClientConf(
            body: body, privateKey: private, address: c['address']! as String);
        expect(conf, c['conf']);
        expect(conf.startsWith('﻿'), isFalse,
            reason: 'a byte order mark makes the stock client reject the file');
        expect(conf.contains('\r'), isFalse);

        // And the key that gets registered is the one the server will see.
        final pair = await tunnelKeyPairFromPrivate(private);
        expect(encodeStd(pair.publicKey), c['public_key']);
      });
    }

    test('the body fields come through', () {
      final body = ConfigBody.parse(cases.first['body']! as String);
      expect(body.endpoint, '203.0.113.9:41820');
      expect(body.region, 'sto');
      expect(body.params.i1, '<r 16>');
      expect(body.params.randomTrailers, isTrue);
      expect(body.params.headerProtectionKey, hasLength(32));
      expect(body.enroll, isNotNull);
      expect(body.enroll!.address, '10.8.0.251/32');
      expect(body.enroll!.server, '10.8.0.1:7843');
      expect(body.allowedIps, hasLength(3));
    });

    test('bad bodies are refused', () {
      final good =
          jsonDecode(cases.first['body']! as String) as Map<String, Object?>;
      String with_(Map<String, Object?> changes) =>
          jsonEncode({...good, ...changes});
      expect(() => ConfigBody.parse('nope'), failsWith('not valid JSON'));
      expect(
          () => ConfigBody.parse(with_({'version': 9})), failsWith('version'));
      expect(() => ConfigBody.parse(with_({'enroll': null})),
          failsWith('register'));
      expect(() => ConfigBody.parse(with_({'endpoint': ''})),
          failsWith('endpoint'));
      expect(() => ConfigBody.parse(with_({'allowed_ips': <String>[]})),
          failsWith('allowed'));
      expect(() => ConfigBody.parse(with_({'server_public_key': ''})),
          failsWith('server public key'));
    });

    test('a version 1 body is still read', () {
      final good =
          jsonDecode(cases.first['body']! as String) as Map<String, Object?>;
      final old =
          ConfigBody.parse(jsonEncode({...good, 'version': 1, 'enroll': null}));
      expect(old.enroll, isNull);
    });
  });

  group('tunnel keys', () {
    test('match the published test vector of the curve', () async {
      // RFC 7748 section 6.1, Alice.
      final private = _unhex(
          '77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a');
      final pair = await tunnelKeyPairFromPrivate(private);
      expect(hex(pair.publicKey),
          '8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a');
    });

    test('a generated pair is clamped and reproducible from its private half',
        () async {
      final pair = await generateTunnelKeyPair();
      expect(pair.privateKey[0] & 7, 0);
      expect(pair.privateKey[31] & 0x80, 0);
      expect(pair.privateKey[31] & 0x40, 0x40);
      final again = await tunnelKeyPairFromPrivate(pair.privateKey);
      expect(again.publicKey, pair.publicKey);
      expect((await generateTunnelKeyPair()).publicKey, isNot(pair.publicKey));
    });

    test('a key of the wrong size is refused', () async {
      await expectLater(
          tunnelKeyPairFromPrivate(Uint8List(31)), failsWith('32 bytes'));
    });
  });
}

Uint8List _unhex(String s) => Uint8List.fromList([
      for (var i = 0; i < s.length; i += 2)
        int.parse(s.substring(i, i + 2), radix: 16)
    ]);
