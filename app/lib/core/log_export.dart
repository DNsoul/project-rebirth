import 'dart:convert';
import 'dart:io';

import 'log_journal.dart';
import 'log_scrub.dart';
import 'version.dart';

/// Saving the journal did not work. The message is for the person.
class LogExportException implements Exception {
  LogExportException(this.message, [this.cause]);

  final String message;

  /// What actually went wrong, for the journal.
  final Object? cause;

  @override
  String toString() => cause == null ? message : '$message ($cause)';
}

/// What a person is told when saving fails for a reason nobody foresaw.
const exportFailedMessage = 'Не удалось сохранить журнал. Повторите ещё раз.';

/// When the system would not let the app write to Downloads (Android 7–9 ask).
const exportDeniedMessage =
    'Не удалось сохранить журнал. Разрешите приложению доступ к файлам и повторите.';

/// When there is no room left.
const exportNoSpaceMessage =
    'Не удалось сохранить журнал. На устройстве не хватает места.';

/// What is said when it worked, above the name of the file.
const exportSavedTitle = 'Журнал сохранён в Загрузки:';

/// Where the saved journal goes: the Downloads folder of the system.
abstract class LogSink {
  /// Saves [bytes] as a file called [name] (or close to it, if that name is
  /// taken) and returns the name it was saved under. Throws
  /// [LogExportException] with words for the person when it cannot.
  Future<String> save(String name, List<int> bytes);
}

/// A folder on disk: Downloads on Windows, any folder in the tests.
class DirectoryLogSink implements LogSink {
  DirectoryLogSink(this.directory);

  /// Looked up when saving, not before: the folder may move.
  final Future<Directory> Function() directory;

  @override
  Future<String> save(String name, List<int> bytes) async {
    try {
      final dir = await directory();
      await dir.create(recursive: true);
      final file = _free(dir, name);
      await file.writeAsBytes(bytes, flush: true);
      return file.uri.pathSegments.last;
    } on FileSystemException catch (e) {
      throw LogExportException(_explain(e), e);
    }
  }

  /// [name], or `name (2).txt` and so on if there is a file called that.
  static File _free(Directory dir, String name) {
    final dot = name.lastIndexOf('.');
    final stem = dot < 0 ? name : name.substring(0, dot);
    final ext = dot < 0 ? '' : name.substring(dot);
    var file = File('${dir.path}${Platform.pathSeparator}$name');
    for (var i = 2; file.existsSync(); i++) {
      file = File('${dir.path}${Platform.pathSeparator}$stem ($i)$ext');
    }
    return file;
  }

  static String _explain(FileSystemException e) {
    final code = e.osError?.errorCode;
    // ENOSPC on Linux and Android, ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL on
    // Windows.
    if (code == 28 || code == 112 || code == 39) return exportNoSpaceMessage;
    return exportFailedMessage;
  }
}

/// `rebirth-logs-20261114-093005.txt` for that moment.
String exportFileName(DateTime t) {
  String two(int n) => n.toString().padLeft(2, '0');
  return 'rebirth-logs-${t.year}${two(t.month)}${two(t.day)}'
      '-${two(t.hour)}${two(t.minute)}${two(t.second)}.txt';
}

// An entry starts with the time: 2026-11-14T09:30:05.123.
final _stamp = RegExp(r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d');

/// The journals put together into one, in the order things happened. Each of
/// them is in order already; lines without a time of their own (a stack trace)
/// stay with the line before them. When two entries have the same time to the
/// millisecond, the journal given first goes first.
String mergeJournals(List<String> journals) {
  final sources = [for (final j in journals) _entries(j)];
  final at = List.filled(sources.length, 0);
  final out = StringBuffer();
  while (true) {
    var pick = -1;
    for (var i = 0; i < sources.length; i++) {
      if (at[i] >= sources[i].length) continue;
      if (pick < 0 ||
          _key(sources[i][at[i]]).compareTo(_key(sources[pick][at[pick]])) <
              0) {
        pick = i;
      }
    }
    if (pick < 0) break;
    out.write(sources[pick][at[pick]++]);
  }
  return out.toString();
}

String _key(String entry) => entry.length >= 23 ? entry.substring(0, 23) : '';

List<String> _entries(String text) {
  final out = <String>[];
  for (final line in const LineSplitter().convert(text)) {
    if (line.isEmpty) continue;
    if (out.isEmpty || _stamp.hasMatch(line)) {
      out.add('$line\n');
    } else {
      out[out.length - 1] += '$line\n';
    }
  }
  return out;
}

/// Saves the journal into Downloads as one file: a few lines about the app and
/// the device, then the app's own journal and the system side's (on Android:
/// the VPN service and the tunnel library), merged in the order things
/// happened.
class LogExporter {
  LogExporter({
    required this.journal,
    required this.sink,
    required this.device,
    this.otherJournals = const [],
    this.state,
    DateTime Function()? clock,
  }) : _clock = clock ?? DateTime.now;

  final LogJournal journal;

  /// The paths of other journals of the same format (with their `.1` copies).
  final List<String> otherJournals;

  final LogSink sink;

  /// What the device is: "Xiaomi Redmi Note 12, Android 13 (API 33)".
  final Future<String> Function() device;

  /// What the connection is doing right now, in a line.
  final String Function()? state;

  final DateTime Function() _clock;

  LogScrubber get _scrubber => journal.scrubber;

  /// The whole text of the file, header first.
  Future<String> build([DateTime? at]) async {
    final now = at ?? _clock();
    var dev = '';
    try {
      dev = await device();
    } on Object catch (e) {
      dev = '? ($e)';
    }
    final offset = now.timeZoneOffset;
    final sign = offset.isNegative ? '-' : '+';
    final h = offset.inHours.abs().toString().padLeft(2, '0');
    final m = (offset.inMinutes.abs() % 60).toString().padLeft(2, '0');
    final header = StringBuffer()
      ..writeln('Rebirth $appVersion (build $appBuild)')
      ..writeln('device: $dev')
      ..writeln('saved: ${now.toIso8601String()} UTC$sign$h:$m');
    final s = state?.call();
    if (s != null && s.isNotEmpty) header.writeln('state: $s');
    header.writeln('-' * 40);

    final merged = mergeJournals([
      journal.readAll(),
      for (final p in otherJournals) readRotated(p),
    ]);
    // Scrubbed again: the system side's journal was written by other code, and
    // the secrets of this build are only known here.
    final body = const LineSplitter()
        .convert(merged)
        .map(_scrubber.scrub)
        .join('\n');
    return '${_scrubber.scrub(header.toString())}$body\n';
  }

  /// Saves the file and returns the name it was saved under.
  Future<String> export() async {
    final now = _clock();
    final String text;
    try {
      text = await build(now);
    } on Object catch (e) {
      throw LogExportException(exportFailedMessage, e);
    }
    try {
      return await sink.save(exportFileName(now), utf8.encode(text));
    } on LogExportException {
      rethrow;
    } on Object catch (e) {
      throw LogExportException(exportFailedMessage, e);
    }
  }
}

/// Saves the journal with [exporter], and says in the journal what came of it
/// (after the fact: the line is in the next file saved, not this one).
Future<String> saveJournal(
  LogExporter exporter,
  void Function(String line) log,
) async {
  log('journal: saving');
  try {
    final name = await exporter.export();
    log('journal: saved as $name');
    return name;
  } on LogExportException catch (e) {
    log('journal: not saved: $e');
    rethrow;
  }
}
