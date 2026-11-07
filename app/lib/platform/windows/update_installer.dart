import 'dart:io';

import '../../core/updater.dart';

/// Starts a program apart from this one, so that it keeps running when this
/// one ends. Replaced in tests.
typedef Starter = Future<void> Function(String exe, List<String> args);

Future<void> startDetached(String exe, List<String> args) async {
  await Process.start(exe, args, mode: ProcessStartMode.detached);
}

/// Runs the downloaded setup program and gets out of its way.
///
/// The setup program has to replace the files of the program that started it, so
/// this one leaves as soon as the setup is running: the tunnel is taken down and
/// the names given back first, which is what [leave] does. The setup starts the
/// program again when it is done. It is started silently with a progress
/// window, and without a prompt for rights: this program already has them, and
/// what it starts inherits them.
class WindowsUpdateInstaller implements UpdateInstaller {
  WindowsUpdateInstaller({
    required this.leave,
    Starter? start,
    void Function(String line)? log,
  }) : _start = start ?? startDetached,
       _log = log ?? ((_) {});

  /// Ends this program, cleanly. Does not return in the real program.
  final Future<void> Function() leave;

  final Starter _start;
  final void Function(String) _log;

  /// What the setup program is told. `/SILENT` shows only its progress window,
  /// the rest keeps it from asking questions or restarting the machine.
  static const arguments = ['/SILENT', '/SUPPRESSMSGBOXES', '/NORESTART'];

  @override
  Future<void> install(File file) async {
    try {
      await _start(file.path, arguments);
    } on Object catch (e) {
      throw UpdateException('Не удалось запустить установщик: $e');
    }
    _log('update: setup started, leaving');
    await leave();
  }
}
