import 'dart:convert';
import 'dart:io';

import 'log_scrub.dart';

/// The app's journal: every line the app logs, with the time, in a file in its
/// own folder, so that what happened can be looked at later on a device with no
/// cable and no developer tools. Long-pressing the button saves it, together with
/// the Android side's journal, into Downloads (`log_export.dart`).
///
/// The file does not grow without end. When it reaches [maxBytes] it becomes
/// `<name>.1` (the one before is dropped) and a new one is started, so the
/// journal never takes more than twice that. Every line goes through [scrubber]
/// first: keys, tokens and the servers' addresses never reach the disk.
class LogJournal {
  LogJournal(
    this.path, {
    LogScrubber? scrubber,
    this.maxBytes = 1024 * 1024,
    DateTime Function()? clock,
    this.echo,
  }) : scrubber = scrubber ?? LogScrubber(),
       _clock = clock ?? DateTime.now {
    _open();
  }

  /// The current file; the previous one is [path] with `.1` added.
  final String path;
  final LogScrubber scrubber;
  final int maxBytes;
  final DateTime Function() _clock;

  /// Also given each line, already scrubbed (for `adb logcat`, on Android).
  final void Function(String line)? echo;

  int _size = 0;

  String get previousPath => '$path.1';

  void _open() {
    try {
      final f = File(path);
      f.parent.createSync(recursive: true);
      _size = f.existsSync() ? f.lengthSync() : 0;
      // A file far over the limit is from before there was one: what is in it
      // was never scrubbed, and nobody is going to read that much.
      if (_size > maxBytes * 2) {
        f.deleteSync();
        _size = 0;
      } else if (_size >= maxBytes) {
        _rotate();
      }
    } on FileSystemException {
      _size = 0;
    }
  }

  void _rotate() {
    final old = File(previousPath);
    if (old.existsSync()) old.deleteSync();
    final f = File(path);
    if (f.existsSync()) f.renameSync(previousPath);
    _size = 0;
  }

  /// Adds [line] with the time in front. A line with line breaks in it (a stack
  /// trace) is kept together: only its first line has the time.
  void write(String line) {
    final clean = scrubber.scrub(line);
    echo?.call(clean);
    final entry = '${_clock().toIso8601String()} $clean\n';
    try {
      final bytes = utf8.encode(entry);
      if (_size > 0 && _size + bytes.length > maxBytes) {
        _rotate();
      }
      File(path).writeAsBytesSync(bytes, mode: FileMode.append);
      _size += bytes.length;
    } on FileSystemException {
      // The journal is a help; losing a line is not worth stopping for.
    }
  }

  /// Everything in the journal, the older file first.
  String readAll() => readRotated(path);
}

/// The text of [path] with its rotated copy (`<path>.1`) in front, or nothing
/// for files that are not there.
String readRotated(String path) {
  final out = StringBuffer();
  for (final p in ['$path.1', path]) {
    try {
      final f = File(p);
      if (f.existsSync()) {
        // A crash can cut a character in half; the rest still decodes.
        out.write(utf8.decode(f.readAsBytesSync(), allowMalformed: true));
      }
    } on FileSystemException {
      // Unreadable: the rest is still worth having.
    }
  }
  return out.toString();
}
