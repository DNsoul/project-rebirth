import 'dart:async';
import 'dart:io';

import 'package:awg_proto/awg_proto.dart';

/// Where the announcement of the newest version comes from. The connection
/// controller reads it from the gist along with the configuration.
abstract class ReleaseSource {
  /// The newest announcement that passed verification, or null if there is none.
  Release? get release;

  /// Fires when a different announcement has been accepted.
  Stream<Release> get releaseStream;
}

/// Saves what is at an address into a file.
abstract class Downloader {
  /// Writes the body at [url] to [file]. Throws [UpdateException] if the server
  /// does not answer properly, and stops at once if more than [limit] bytes
  /// arrive: the announcement says how big the file is, and a server that sends
  /// more is not sending that file. [onProgress] is told how much has arrived.
  Future<void> download(
    Uri url,
    File file, {
    required int limit,
    void Function(int received)? onProgress,
  });
}

/// Hands a downloaded installer over to the system.
abstract class UpdateInstaller {
  /// Starts [file]. On Windows the program leaves afterwards, so that the
  /// installer can replace it; on Android the system takes over from here.
  Future<void> install(File file);
}

/// Something went wrong with an update. The message is for the user.
class UpdateException implements Exception {
  UpdateException(this.message);

  final String message;

  @override
  String toString() => message;
}

enum UpdatePhase { available, downloading, installing, failed }

/// A version that could be installed in place of this one.
class UpdateOffer {
  const UpdateOffer({
    required this.release,
    required this.asset,
    required this.mandatory,
  });

  final Release release;

  /// The installer for this platform.
  final ReleaseAsset asset;

  /// This build is older than the oldest one that still works against the
  /// servers, so "later" is not on offer.
  final bool mandatory;

  String get version => release.version;
  String get notes => release.notes;
}

class UpdateStatus {
  const UpdateStatus(
    this.phase,
    this.offer, {
    this.progress = 0,
    this.message = '',
  });

  final UpdatePhase phase;
  final UpdateOffer offer;

  /// How much of the download is done, from 0 to 1.
  final double progress;

  /// What went wrong, or a word about what is happening.
  final String message;
}

/// What the screen needs: whether there is a newer version, and what a person
/// can do about it.
abstract class UpdateView {
  /// The offer, or null when this is the newest version or nothing was announced.
  UpdateStatus? get status;
  Stream<UpdateStatus?> get statusStream;

  /// Downloads the new version, checks it is the one that was announced, and
  /// starts its installer.
  Future<void> install();
}

/// Turns announcements into an offer, and an accepted offer into a started
/// installer.
///
/// What is trusted is the announcement, which is signed. The file is not: it is
/// only installed if its size and SHA-256 are the ones in the announcement, so
/// the server it comes from, and everything on the way, can be anything at all.
class UpdateController implements UpdateView {
  UpdateController({
    required this.releases,
    required this.os,
    required this.currentBuild,
    required this.downloader,
    required this.installer,
    required this.directory,
    void Function(String line)? log,
  }) : _log = log ?? ((_) {}) {
    _sub = releases.releaseStream.listen((_) => _refresh());
    _refresh();
  }

  final ReleaseSource releases;

  /// `windows` or `android`: which installer in an announcement is ours.
  final String os;

  /// The build this program is.
  final int currentBuild;

  final Downloader downloader;
  final UpdateInstaller installer;

  /// Where a download is kept. Emptied before each one.
  final Directory directory;

  final void Function(String) _log;

  final _controller = StreamController<UpdateStatus?>.broadcast();
  late final StreamSubscription<Release> _sub;
  UpdateStatus? _status;
  bool _busy = false;

  @override
  UpdateStatus? get status => _status;

  @override
  Stream<UpdateStatus?> get statusStream => _controller.stream;

  void _emit(UpdateStatus? s) {
    _status = s;
    if (!_controller.isClosed) {
      _controller.add(s);
    }
  }

  /// The offer that follows from what has been announced.
  UpdateOffer? _offer() {
    final r = releases.release;
    if (r == null || r.build <= currentBuild) {
      return null;
    }
    final asset = r.asset(os);
    if (asset == null) {
      return null; // announced for another platform only
    }
    return UpdateOffer(
      release: r,
      asset: asset,
      mandatory: currentBuild < r.minBuild,
    );
  }

  void _refresh() {
    if (_busy) {
      return; // a download in progress is not interrupted by a newer notice
    }
    final offer = _offer();
    if (offer == null) {
      _emit(null);
      return;
    }
    final now = _status;
    if (now != null &&
        now.offer.release.build == offer.release.build &&
        now.phase != UpdatePhase.available) {
      return; // already showing what happened to this very offer
    }
    _emit(UpdateStatus(UpdatePhase.available, offer));
  }

  @override
  Future<void> install() async {
    final start = _status;
    if (_busy || start == null || start.phase == UpdatePhase.installing) {
      return;
    }
    final offer = start.offer;
    _busy = true;
    File? part;
    try {
      _emit(UpdateStatus(UpdatePhase.downloading, offer));
      final file = await _prepare(offer);
      part = File('${file.path}.part');

      var shown = 0.0;
      await downloader.download(
        Uri.parse(offer.asset.url),
        part,
        limit: offer.asset.size,
        onProgress: (received) {
          final p = (received / offer.asset.size).clamp(0.0, 1.0);
          if (p - shown >= 0.01) {
            shown = p;
            _emit(UpdateStatus(UpdatePhase.downloading, offer, progress: p));
          }
        },
      );

      await _verify(part, offer.asset);
      await part.rename(file.path);
      part = null;

      _emit(UpdateStatus(UpdatePhase.installing, offer, progress: 1));
      _log(
        'update: ${offer.version} downloaded and checked, starting ${file.path}',
      );
      await installer.install(file);
      // Still here: the system's own installer took over, or the person turned
      // it down. The offer stays, for the next try.
      _busy = false;
      _emit(UpdateStatus(UpdatePhase.available, offer));
    } on UpdateException catch (e) {
      _fail(offer, e.message);
    } on Object catch (e) {
      _log('update: $e');
      _fail(offer, 'Не удалось обновиться. Повторите позже. ($e)');
    } finally {
      if (part != null) {
        await _quiet(part.delete);
      }
    }
  }

  void _fail(UpdateOffer offer, String message) {
    _log('update failed: $message');
    _busy = false;
    _emit(UpdateStatus(UpdatePhase.failed, offer, message: message));
  }

  Future<File> _prepare(UpdateOffer offer) async {
    await directory.create(recursive: true);
    // Only the file about to be fetched is wanted: whatever an earlier try left
    // is either this file or something that is now out of date.
    await for (final e in directory.list()) {
      await _quiet(() => e.delete(recursive: true));
    }
    final ext = offer.asset.os == osAndroid ? 'apk' : 'exe';
    return File(
      '${directory.path}${Platform.pathSeparator}'
      'Rebirth-${offer.version}.$ext',
    );
  }

  /// The file is the announced one, or it is not installed.
  Future<void> _verify(File file, ReleaseAsset asset) async {
    final size = await file.length();
    if (size != asset.size) {
      throw UpdateException(
        'Скачанный файл не тот, что объявлен. Его размер $size байт вместо '
        '${asset.size}, установка отменена. Повторите позже.',
      );
    }
    final hash = await sha256Hex(file.openRead());
    if (hash != asset.sha256) {
      throw UpdateException(
        'Скачанный файл не прошёл проверку. Его контрольная сумма не совпала '
        'с подписанной, установка отменена. Повторите позже.',
      );
    }
  }

  Future<void> _quiet(FutureOr<void> Function() step) async {
    try {
      await step();
    } on Object catch (e) {
      _log('update: cleanup: $e');
    }
  }

  Future<void> dispose() async {
    await _sub.cancel();
    await _controller.close();
  }
}
