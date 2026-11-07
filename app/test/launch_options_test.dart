import 'package:awg_client/core/launch_options.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('started with nothing, it stays', () {
    expect(LaunchOptions.parse(const []).quitAfter, isNull);
  });

  test('--quit-after=N is read, in seconds', () {
    expect(
      LaunchOptions.parse(const ['--quit-after=7']).quitAfter,
      const Duration(seconds: 7),
    );
    expect(
      LaunchOptions.parse(const ['x', '--quit-after=0', 'y']).quitAfter,
      Duration.zero,
    );
  });

  test('--autostart means hidden and connecting, and nothing else does', () {
    expect(LaunchOptions.parse(const []).autostart, isFalse);
    expect(LaunchOptions.parse(const ['--autostart']).autostart, isTrue);
    expect(
      LaunchOptions.parse(const ['--quit-after=3', '--autostart']).autostart,
      isTrue,
    );
    for (final other in [
      '--autostart=1',
      '-autostart',
      'autostart',
      '--quit',
    ]) {
      expect(LaunchOptions.parse([other]).autostart, isFalse, reason: other);
    }
  });

  test('anything that is not a sensible number is ignored, not obeyed', () {
    for (final bad in [
      '--quit-after=',
      '--quit-after=abc',
      '--quit-after=-5',
      '--quit-after=99999',
      '--quit-after',
      '--quit=5',
    ]) {
      expect(LaunchOptions.parse([bad]).quitAfter, isNull, reason: bad);
    }
  });
}
