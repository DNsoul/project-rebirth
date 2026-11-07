import 'dart:io';

import 'package:flutter/services.dart';

import '../../core/updater.dart';

/// What only Android can do to install a package: show it to the system's
/// installer. The Kotlin side is MainActivity.installApk.
abstract class ApkBridge {
  /// `started` once the system's installer is showing the package, or
  /// `needs_permission` after opening the settings page where the person allows
  /// this app to install packages.
  Future<String> installApk(String path);
}

class MethodChannelApkBridge implements ApkBridge {
  // Shares the channel of the VPN bridge, which is where the Kotlin side listens.
  // Only calls are made on it; the handler for calls the other way is the VPN
  // bridge's.
  MethodChannelApkBridge([MethodChannel? channel])
    : _channel = channel ?? const MethodChannel('awg/vpn');

  final MethodChannel _channel;

  @override
  Future<String> installApk(String path) async =>
      await _channel.invokeMethod<String>('installApk', {'path': path}) ?? '';
}

class AndroidUpdateInstaller implements UpdateInstaller {
  AndroidUpdateInstaller(this.bridge);

  final ApkBridge bridge;

  @override
  Future<void> install(File file) async {
    final String answer;
    try {
      answer = await bridge.installApk(file.path);
    } on PlatformException catch (e) {
      throw UpdateException(
        'Не удалось передать обновление установщику: ${e.message ?? e.code}',
      );
    }
    if (answer == 'needs_permission') {
      throw UpdateException(
        'Чтобы установить обновление, разрешите это приложению: в открывшихся '
        'настройках включите «Установка неизвестных приложений», вернитесь '
        'и нажмите «Повторить».',
      );
    }
  }
}
