import 'package:flutter/services.dart';

import '../../core/log_export.dart';
import 'vpn_bridge.dart';

/// Downloads on Android. From Android 10 the file goes in through MediaStore,
/// which needs no permission; on Android 7–9 the system asks once for the right
/// to write to the shared storage, and the file is written into Downloads
/// directly. The Kotlin side is `Downloads.kt`.
class AndroidDownloadsSink implements LogSink {
  AndroidDownloadsSink([MethodChannel? channel])
    : _channel =
          channel ?? const MethodChannel(MethodChannelVpnBridge.channelName);

  final MethodChannel _channel;

  @override
  Future<String> save(String name, List<int> bytes) async {
    try {
      final saved = await _channel.invokeMethod<String>('saveToDownloads', {
        'name': name,
        'bytes': Uint8List.fromList(bytes),
      });
      return saved == null || saved.isEmpty ? name : saved;
    } on PlatformException catch (e) {
      throw LogExportException(switch (e.code) {
        'denied' => exportDeniedMessage,
        'no_space' => exportNoSpaceMessage,
        _ => exportFailedMessage,
      }, '${e.code}: ${e.message}');
    }
  }
}

/// "Android 13 (API 33)".
Future<String> androidVersion([MethodChannel? channel]) async =>
    await (channel ?? const MethodChannel(MethodChannelVpnBridge.channelName))
        .invokeMethod<String>('androidVersion') ??
    '';

/// The paths of the Android side's journal (the VPN service, the network
/// watcher, the tunnel library), written by `NativeLog.kt` in the same format.
String nativeJournalPath(String dataDir) => '$dataDir/native.log';
