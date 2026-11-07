/// What the program was started with.
///
/// `--autostart` is how the task that starts the program when someone signs in
/// to Windows starts it: the window stays hidden (the native side sees the same
/// switch), and the program connects by itself, since nobody is there to press
/// the button.
///
/// `--quit-after=N` is for checking the program, not for using it: it makes the
/// program leave by the same way the menu's "Выход" does, after N seconds. The
/// way out of the program is a menu on an icon that nothing but a person's click
/// can open, and it is the one path that takes the tunnel down and gives the
/// machine its names back, so it has to be possible to run it unattended.
///
/// (`--quit` is not here: it never reaches Dart. The native side handles it
/// before any of this starts. See `main.cpp`.)
class LaunchOptions {
  const LaunchOptions({this.quitAfter, this.autostart = false});

  factory LaunchOptions.parse(List<String> args) {
    Duration? quitAfter;
    for (final arg in args) {
      const prefix = '--quit-after=';
      if (arg.startsWith(prefix)) {
        final seconds = int.tryParse(arg.substring(prefix.length));
        if (seconds != null && seconds >= 0 && seconds <= 3600) {
          quitAfter = Duration(seconds: seconds);
        }
      }
    }
    return LaunchOptions(
      quitAfter: quitAfter,
      autostart: args.contains('--autostart'),
    );
  }

  /// When to leave by itself, or null to stay until told to go.
  final Duration? quitAfter;

  /// Started with the system: hidden, and connecting.
  final bool autostart;
}
