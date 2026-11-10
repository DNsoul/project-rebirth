import 'dart:async';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/semantics.dart';
import 'package:flutter/services.dart';

import 'app.dart';
import 'core/app_state.dart';
import 'core/bootstrap.dart';
import 'core/controller.dart';
import 'core/device_label.dart';
import 'core/dns_probe.dart';
import 'core/gist_source.dart';
import 'core/http_downloader.dart';
import 'core/stall_watchdog.dart';
import 'core/udp_enroll_transport.dart';
import 'core/updater.dart';
import 'core/version.dart';
import 'package:awg_proto/awg_proto.dart' show osAndroid;
import 'platform/android/android_system.dart';
import 'platform/android/apk_installer.dart';
import 'platform/android/android_tunnel.dart';
import 'platform/android/awg_native.dart';
import 'platform/android/permissions.dart';
import 'platform/android/vpn_bridge.dart';
import 'platform/windows/tray_state.dart' show trayStateFor;

/// The Android app: the same connecting logic as on Windows, with the system's
/// VPN interface and the tunnel library in place of the installed client.
Future<void> runAndroid(List<String> args) async {
  WidgetsFlutterBinding.ensureInitialized();
  SemanticsBinding.instance.ensureSemantics();

  final bridge = MethodChannelVpnBridge();
  final dataDir = await bridge.filesDir();
  final logFile = File('$dataDir/app.log');
  void log(String line) {
    debugPrint('awg: $line'); // visible with: adb logcat -s flutter
    try {
      logFile.writeAsStringSync(
        '${DateTime.now().toIso8601String()} $line\n',
        mode: FileMode.append,
      );
    } on FileSystemException {
      // The log is a convenience; losing a line is not worth stopping for.
    }
  }

  FlutterError.onError = (details) =>
      log('flutter: ${details.exceptionAsString()}');
  PlatformDispatcher.instance.onError = (error, stack) {
    log('unhandled: $error\n$stack');
    return true;
  };

  final Bootstrap bootstrap;
  try {
    bootstrap = Bootstrap.parse(
      await rootBundle.loadString('assets/bootstrap.json'),
    );
  } on Object catch (e) {
    runApp(
      BrokenApp('Сборка приложения неполная: нет данных для доступа.\n$e'),
    );
    return;
  }

  final AwgNative native;
  try {
    native = FfiAwgNative();
  } on Object catch (e) {
    log('tunnel library: $e');
    runApp(BrokenApp('Не загрузилась библиотека туннеля.\n$e'));
    return;
  }

  // The system says when the phone has moved from one network to another (Wi-Fi
  // to mobile data and back); the controller makes the tunnel follow.
  final networkChanges = StreamController<bool>.broadcast();
  bridge.onNetworkChanged = (available) {
    log('network: ${available ? 'changed' : 'gone'}');
    networkChanges.add(available);
  };

  // What the owner sees next to this phone's request to come in.
  var model = '';
  try {
    model = await bridge.deviceModel();
  } on Object catch (e) {
    log('device model: $e');
  }

  final controller = ConnectionController(
    bootstrap: bootstrap,
    gist: gistSourceFor(bootstrap),
    store: FileStateStore('$dataDir/state.json'),
    tunnel: AndroidTunnelDriver(native: native, bridge: bridge, log: log),
    dns: AndroidDnsGuard(),
    probe: UdpDnsProbe(),
    machine: AndroidMachineId(bridge),
    transportFactory: () => UdpEnrollTransport(log: log),
    deviceName: deviceLabel(model, fallback: 'Android'),
    networkChanges: networkChanges.stream,
    log: log,
  );

  // A newer version is offered in the window and handed to the system's installer.
  final updates = UpdateController(
    releases: controller,
    os: osAndroid,
    currentBuild: appBuild,
    downloader: HttpDownloader(),
    installer: AndroidUpdateInstaller(MethodChannelApkBridge()),
    directory: Directory('$dataDir/updates'),
    mirrors: bootstrap.releaseMirrors,
    log: log,
  );

  // The system took the VPN away (the user withdrew the permission, or another
  // VPN app started): the screen goes back to "disconnected" instead of
  // claiming a connection that is not there.
  bridge.onRevoked = () {
    log('the system revoked the VPN');
    unawaited(controller.disconnect());
  };

  // The notification says what the app is doing, in the same words the tray icon
  // uses on Windows.
  String? shown;
  void tell(AppStatus s) {
    final line = trayStateFor(s).statusLine;
    if (line == shown) return;
    shown = line;
    unawaited(
      bridge.setStatus(line).catchError((Object e) => log('notification: $e')),
    );
  }

  controller.statusStream.listen(tell);

  runApp(App(controller: controller, updates: updates));

  StallWatchdog(log: log).start();
  // First what the person has to allow, one question at a time: the notification
  // (here), then the VPN (in the controller's start-up check). The window is
  // already up behind the questions.
  unawaited(() async {
    await askForPermissions(bridge, log: log);
    try {
      await controller.start();
    } on Object catch (e, s) {
      log('start failed: $e\n$s');
    }
  }());
}
