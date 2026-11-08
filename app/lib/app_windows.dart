import 'dart:async';
import 'dart:io';
import 'dart:ui';

import 'package:flutter/material.dart';
import 'package:flutter/semantics.dart';
import 'package:flutter/services.dart';

import 'app.dart';
import 'core/app_state.dart';
import 'core/bootstrap.dart';
import 'core/controller.dart';
import 'core/dns_probe.dart';
import 'core/gist_source.dart';
import 'core/http_downloader.dart';
import 'core/launch_options.dart';
import 'core/logon_connect.dart';
import 'core/stall_watchdog.dart';
import 'core/udp_enroll_transport.dart';
import 'core/updater.dart';
import 'core/version.dart';
import 'package:awg_proto/awg_proto.dart' show osWindows;
import 'platform/windows/single_instance.dart';
import 'platform/windows/tray.dart';
import 'platform/windows/update_installer.dart';
import 'platform/windows/windows_system.dart';
import 'platform/windows/windows_tunnel.dart';

Future<void> runWindows(List<String> args) async {
  WidgetsFlutterBinding.ensureInitialized();
  // The screen is one button and a few words, so describing it to assistive
  // technology (a screen reader, or any program that drives the window through
  // accessibility) costs nothing; without this Flutter only does it once it has
  // noticed that something is listening.
  SemanticsBinding.instance.ensureSemantics();
  final options = LaunchOptions.parse(args);

  final dataDir = '${Platform.environment['LOCALAPPDATA'] ?? '.'}\\awg-rotator';
  final logFile = File('$dataDir\\app.log');
  void log(String line) {
    try {
      logFile.parent.createSync(recursive: true);
      logFile.writeAsStringSync(
        '${DateTime.now().toIso8601String()} $line\n',
        mode: FileMode.append,
      );
    } on FileSystemException {
      // The log is a convenience; losing a line is not worth stopping for.
    }
  }

  // Whatever goes wrong without anyone having asked for it ends up in the log
  // instead of silently taking the app with it.
  FlutterError.onError = (details) =>
      log('flutter: ${details.exceptionAsString()}');
  PlatformDispatcher.instance.onError = (error, stack) {
    log('unhandled: $error\n$stack');
    return true;
  };

  // First of all, before anything touches a tunnel: a second copy would remove
  // the first one's tunnels on start. Starting the program again shows the
  // window of the copy that is running.
  final instance = await SingleInstance.acquire('$dataDir\\app.lock');
  if (instance == null) {
    // Started with the system while it is already running: nothing to show.
    if (!options.autostart) {
      bringRunningWindowToFront();
    }
    exit(0);
  }

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

  final controller = ConnectionController(
    bootstrap: bootstrap,
    gist: gistSourceFor(bootstrap),
    store: FileStateStore('$dataDir\\state.json'),
    tunnel: WindowsTunnelDriver(log: log),
    dns: WindowsDnsGuard(),
    probe: UdpDnsProbe(),
    machine: WindowsMachineId(),
    transportFactory: () => UdpEnrollTransport(log: log),
    deviceName: 'windows',
    log: log,
  );

  late final NativeTray tray;
  var quitting = false;
  // The only way out. The tunnel is taken down first and the machine gets its
  // names back; if that hangs, it is given up on after a while rather than
  // leaving a program that cannot be closed.
  Future<void> quit() async {
    if (quitting) {
      return;
    }
    quitting = true;
    log('quitting');
    try {
      await controller.dispose().timeout(const Duration(seconds: 20));
    } on Object catch (e) {
      log('quit: $e');
    }
    log('quit: tunnel down');
    await tray.dispose();
    log('quit: icon removed');
    await instance.release();
    // By now the tunnel is down, the icon is gone and the lock is released, so
    // the process just ends. Asking the window to close itself is not needed, and
    // a plugin that tried it crashed the engine on the way out.
    log('quit: exiting');
    exit(0);
  }

  // A newer version is offered in the window. Installing it means leaving: the
  // setup replaces the files of this program, so it is started and this goes.
  final updates = UpdateController(
    releases: controller,
    os: osWindows,
    currentBuild: appBuild,
    downloader: HttpDownloader(),
    installer: WindowsUpdateInstaller(leave: quit, log: log),
    directory: Directory('$dataDir\\updates'),
    mirrors: bootstrap.releaseMirrors,
    log: log,
  );

  tray = NativeTray(view: controller, onQuit: quit, log: log);
  await tray.init();

  // The window comes first. Everything slow (looking for what a previous run left
  // behind, installing the start-up guard) happens behind it, and a connection
  // asked for meanwhile simply waits its turn.
  runApp(App(controller: controller, updates: updates));

  StallWatchdog(log: log).start();
  unawaited(
    controller
        .start()
        .catchError((Object e, StackTrace s) {
          // The window is already open: the button reports what is wrong when pressed.
          log('start failed: $e\n$s');
        })
        .then((_) {
          // Started with the system: nobody is there to press the button.
          if (options.autostart) {
            log('started with the system: connecting');
            return connectAtLogon(controller, log: log);
          }
        }),
  );
  unawaited(
    WindowsBootGuard().ensureInstalled().catchError(
      (Object e) => log('boot guard: $e'),
    ),
  );

  final leaveAfter = options.quitAfter;
  if (leaveAfter != null) {
    log('will quit by itself in ${leaveAfter.inSeconds}s');
    Timer(leaveAfter, () => unawaited(quit()));
  }
}
