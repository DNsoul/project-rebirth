import 'dart:ffi';
import 'dart:io';

import 'package:ffi/ffi.dart';

/// The Downloads folder of the person who is signed in to Windows: where
/// Explorer has it (it can be moved to another drive), or
/// `%USERPROFILE%\Downloads` if Windows does not say.
Directory windowsDownloads() {
  final known = _knownDownloads();
  if (known != null && known.isNotEmpty) return Directory(known);
  final home = Platform.environment['USERPROFILE'] ?? '.';
  return Directory('$home\\Downloads');
}

final class _Guid extends Struct {
  @Uint32()
  external int data1;
  @Uint16()
  external int data2;
  @Uint16()
  external int data3;
  @Array(8)
  external Array<Uint8> data4;
}

/// SHGetKnownFolderPath(FOLDERID_Downloads), or null.
String? _knownDownloads() {
  try {
    final shell = DynamicLibrary.open('shell32.dll');
    final ole = DynamicLibrary.open('ole32.dll');
    final getPath = shell
        .lookupFunction<
          Int32 Function(
            Pointer<_Guid>,
            Uint32,
            IntPtr,
            Pointer<Pointer<Utf16>>,
          ),
          int Function(Pointer<_Guid>, int, int, Pointer<Pointer<Utf16>>)
        >('SHGetKnownFolderPath');
    final free = ole
        .lookupFunction<Void Function(Pointer), void Function(Pointer)>(
          'CoTaskMemFree',
        );
    // {374DE290-123F-4565-9164-39C4925E467B}
    final id = calloc<_Guid>();
    final out = calloc<Pointer<Utf16>>();
    try {
      id.ref
        ..data1 = 0x374DE290
        ..data2 = 0x123F
        ..data3 = 0x4565;
      const tail = [0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B];
      for (var i = 0; i < 8; i++) {
        id.ref.data4[i] = tail[i];
      }
      final rc = getPath(id, 0, 0, out);
      final p = out.value;
      try {
        return rc == 0 && p != nullptr ? p.toDartString() : null;
      } finally {
        // Freed whether it worked or not, as the documentation asks.
        if (p != nullptr) free(p);
      }
    } finally {
      calloc.free(id);
      calloc.free(out);
    }
  } on Object {
    return null;
  }
}
