import 'package:awg_client/core/device_label.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('a model is passed on as it is', () {
    expect(
      deviceLabel('Xiaomi Redmi Note 12', fallback: 'Android'),
      'Xiaomi Redmi Note 12',
    );
  });

  test('nothing usable gives the fallback', () {
    expect(deviceLabel('', fallback: 'Android'), 'Android');
    expect(deviceLabel(' \t\n ', fallback: 'Windows'), 'Windows');
  });

  test('control characters and runs of spaces become single spaces', () {
    expect(deviceLabel('vivo\u0000  V2309\n', fallback: 'x'), 'vivo V2309');
  });

  test('a long name is cut to what the server takes', () {
    final label = deviceLabel(
      'Очень длинное название устройства от производителя',
      fallback: 'x',
    );
    expect(label.runes.length, lessThanOrEqualTo(32));
    expect(label, startsWith('Очень длинное'));
  });
}
