import 'dart:ffi';
import 'dart:io';

import 'package:ffi/ffi.dart';

/// Only one copy of the app may run.
///
/// With the window hidden in the notification area it is easy to start the
/// program a second time, and a second copy is not harmless: on start it removes
/// every tunnel and every name-resolution rule of ours, which would take the
/// first copy's connection down from under it.
///
/// The first copy holds an exclusive lock on a file for as long as it lives. The
/// operating system lets go of it when the process ends, however it ends, so a
/// crash cannot leave the app unable to start.
class SingleInstance {
  SingleInstance._(this._file);

  final RandomAccessFile _file;

  /// Takes the lock, or returns null if another copy already holds it.
  static Future<SingleInstance?> acquire(String lockPath) async {
    final file = File(lockPath);
    await file.parent.create(recursive: true);
    final handle = await file.open(mode: FileMode.write);
    try {
      await handle.lock(FileLock.exclusive);
    } on FileSystemException {
      await handle.close();
      return null;
    }
    return SingleInstance._(handle);
  }

  Future<void> release() async {
    try {
      await _file.unlock();
    } on FileSystemException {
      // Closing releases it anyway.
    }
    await _file.close();
  }
}

/// The class name every window made by the Flutter runner has.
const _windowClass = 'FLUTTER_RUNNER_WIN32_WINDOW';

const _swRestore = 9;

/// Shows the window of the copy that is already running, so that starting the
/// program again looks like opening it. Returns whether there was one to show.
///
/// The runner makes its window before any Dart code runs, so this copy already
/// has a window of the same class, not yet shown. Only a window that belongs to
/// another process counts.
bool bringRunningWindowToFront({String windowClass = _windowClass}) {
  final user32 = DynamicLibrary.open('user32.dll');
  final findWindowEx = user32
      .lookupFunction<
        IntPtr Function(IntPtr, IntPtr, Pointer<Utf16>, Pointer<Utf16>),
        int Function(int, int, Pointer<Utf16>, Pointer<Utf16>)
      >('FindWindowExW');
  final threadProcess = user32
      .lookupFunction<
        Uint32 Function(IntPtr, Pointer<Uint32>),
        int Function(int, Pointer<Uint32>)
      >('GetWindowThreadProcessId');
  final showWindow = user32
      .lookupFunction<Int32 Function(IntPtr, Int32), int Function(int, int)>(
        'ShowWindow',
      );
  final setForeground = user32
      .lookupFunction<Int32 Function(IntPtr), int Function(int)>(
        'SetForegroundWindow',
      );

  final cls = windowClass.toNativeUtf16();
  final owner = calloc<Uint32>();
  try {
    // Every window of the class, hidden ones included: hiding does not destroy.
    var handle = 0;
    while (true) {
      handle = findWindowEx(0, handle, cls, nullptr);
      if (handle == 0) {
        return false;
      }
      threadProcess(handle, owner);
      if (owner.value != pid) {
        break;
      }
    }
    showWindow(handle, _swRestore);
    setForeground(handle);
    return true;
  } finally {
    calloc.free(cls);
    calloc.free(owner);
  }
}
