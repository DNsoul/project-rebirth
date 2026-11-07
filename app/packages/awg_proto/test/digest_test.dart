import 'dart:convert';

import 'package:awg_proto/awg_proto.dart';
import 'package:test/test.dart';

void main() {
  test('is the SHA-256 of the text, from the published vector', () async {
    expect(
      await sha256Hex(Stream.value(utf8.encode('abc'))),
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
    );
  });

  test('does not care where the chunks are cut', () async {
    final whole = await sha256Hex(Stream.value(utf8.encode('abcdefghij')));
    final cut = await sha256Hex(
      Stream.fromIterable([
        utf8.encode('a'),
        utf8.encode('bcdef'),
        <int>[],
        utf8.encode('ghij'),
      ]),
    );
    expect(cut, whole);
  });

  test('knows the hash of nothing', () async {
    expect(
      await sha256Hex(const Stream<List<int>>.empty()),
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    );
  });
}
