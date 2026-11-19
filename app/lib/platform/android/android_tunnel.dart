import 'dart:async';

import '../../core/platform.dart';
import 'awg_native.dart';
import 'conf_plan.dart';
import 'ru_apps.dart';
import 'vpn_bridge.dart';

/// Brings tunnels up on Android: the system opens a VPN interface for the app,
/// and the tunnel library (the same amneziawg-go the servers run) moves the
/// packets through it.
///
/// Android lets an app hold one VPN interface at a time. Opening a new one
/// replaces the old, so a move from one server to the next costs a short gap
/// instead of overlapping the two, and the old tunnel is cleaned up afterwards
/// the way it is on Windows.
///
/// Russian apps are left out of the interface (see ru_apps.dart). Which ones is
/// settled when an interface is opened, so an app installed later is let past
/// the tunnel once the interface is opened again: [interfaceOutdated] asks for
/// that.
class AndroidTunnelDriver implements TunnelDriver {
  AndroidTunnelDriver({
    required this.native,
    required this.bridge,
    this.log,
    this.session = 'Rebirth',
  }) {
    bridge.onAppsChanged = () => unawaited(_appsChanged());
  }

  final AwgNative native;
  final VpnBridge bridge;
  final void Function(String line)? log;

  /// What the system calls the connection in its status bar and settings.
  final String session;

  /// The tunnels that are running, by name, with the library's handle for each.
  final Map<String, int> _handles = {};

  static final _validName = RegExp(r'^awgr-[a-z0-9-]{1,24}$');

  /// The apps to let past the tunnel, worked out once and again after an app is
  /// installed or removed; null until then.
  List<String>? _past;

  /// The apps the open interface leaves out, or null when none is open.
  List<String>? _openedWith;

  final _outdated = StreamController<void>.broadcast();

  /// The apps to let past the tunnel, or null when the phone would not say
  /// what is installed.
  Future<List<String>?> _appsPastTunnel() async {
    final known = _past;
    if (known != null) return known;
    try {
      final picked = appsPastTunnel(await bridge.installedApps());
      log?.call('apps past the tunnel: ${picked.describe()}');
      return _past = picked.packages;
    } on Object catch (e) {
      log?.call('could not list the installed apps: $e');
      return null;
    }
  }

  Future<void> _appsChanged() async {
    _past = null;
    if (await isInterfaceOutdated()) _outdated.add(null);
  }

  @override
  Stream<void> get interfaceOutdated => _outdated.stream;

  /// Only a list that could be made and differs counts: a phone that would not
  /// say what is installed is no reason to let every app back in.
  @override
  Future<bool> isInterfaceOutdated() async {
    final opened = _openedWith;
    if (opened == null || _handles.isEmpty) return false;
    final now = await _appsPastTunnel();
    return now != null && !sameApps(now, opened);
  }

  @override
  Future<String?> preflight() async {
    try {
      final v = native.version();
      log?.call('tunnel library: $v');
    } on Object catch (e) {
      return 'Не загрузилась библиотека туннеля. Переустановите Rebirth. ($e)';
    }
    final bool granted;
    try {
      granted = await bridge.prepare();
    } on Object catch (e) {
      return 'Не удалось запросить разрешение на VPN. Повторите попытку. ($e)';
    }
    return granted
        ? null
        : 'Нужно разрешение на VPN. Нажмите кнопку ещё раз и согласитесь в окне Android.';
  }

  /// The system gives an app one VPN interface; opening one closes the one before.
  @override
  bool get oneAtATime => true;

  @override
  bool get rebindMakesHandshake => true;

  @override
  Future<void> install({required String name, required String conf}) async {
    _checkName(name);
    final plan = planFromConf(conf);

    // A tunnel of the same name is stopped first: it would otherwise go on
    // holding a descriptor to an interface that is about to be replaced. The
    // service stays: an interface is opened again at once, and letting the
    // service go in between would take the key off the status bar for a moment.
    final same = _handles.remove(name);
    if (same != null) {
      native.turnOff(same);
      log?.call('tunnel $name is down');
    }

    // When this driver has no tunnel of its own, whatever the library is still
    // running belongs to nobody: it was started by a program that has since gone
    // (the screen is rebuilt, or the app restarted inside a process that stayed).
    // Left alone, it goes on talking to the server with the same key as the new
    // one, and the server answers whichever of the two spoke last, so half of
    // what comes back is lost.
    if (_handles.isEmpty) {
      final strays = native.turnOffAll();
      if (strays > 0) {
        log?.call('stopped $strays tunnel(s) that nobody owned');
      }
    }

    final past = await _appsPastTunnel() ?? const <String>[];
    final int fd;
    try {
      fd = await bridge.establish(
        session: session,
        addresses: plan.addresses,
        routes: plan.routes,
        dns: plan.dns,
        mtu: plan.mtu,
        excluded: past,
      );
      _openedWith = past;
    } on Object catch (e) {
      if (_handles.isEmpty) await bridge.release();
      throw TunnelException(
        'Android не открыл VPN-интерфейс. Повторите; если не помогает, отключите другие VPN. ($e)',
      );
    }

    final handle = native.turnOn(conf, fd);
    if (handle <= 0) {
      // The library closes the descriptor when it cannot use it, so only the
      // service is left to let go of.
      if (_handles.isEmpty) await bridge.release();
      throw TunnelException(
        'Не удалось запустить туннель. Повторите попытку. (${native.lastError()})',
      );
    }
    _handles[name] = handle;

    await _keepSocketsOutOfTheTunnel(handle);
    log?.call('tunnel $name is up (handle $handle)');
  }

  /// Its sockets must not be sent into the tunnel they carry. With the routes
  /// this app uses they would not be, but a configuration may route everything.
  Future<void> _keepSocketsOutOfTheTunnel(int handle) async {
    for (final socket in [native.socketV4(handle), native.socketV6(handle)]) {
      if (socket >= 0 && !await bridge.protect(socket)) {
        log?.call('could not keep socket $socket out of the tunnel');
      }
    }
  }

  @override
  Future<void> uninstall(String name) async {
    _checkName(name);
    final handle = _handles.remove(name);
    if (handle == null) return;
    native.turnOff(handle);
    if (_handles.isEmpty) {
      await bridge.release();
      _openedWith = null;
    }
    log?.call('tunnel $name is down');
  }

  @override
  Future<void> rebind(String name) async {
    _checkName(name);
    final handle = _handles[name];
    if (handle == null) return;
    final code = native.rebind(handle);
    if (code != 0) {
      throw TunnelException(
        'Не удалось заново открыть сокеты туннеля. (${native.lastError()})',
      );
    }
    // The sockets are new, and the system has to be told about them again.
    await _keepSocketsOutOfTheTunnel(handle);
    log?.call('tunnel $name: sockets reopened for the new network');
  }

  /// What the library counts. Where the phone sends traffic is not in question
  /// here: the interface leads into whichever tunnel opened it last.
  @override
  Future<String> diagnose(String name, String address) async {
    final handle = _handles[name];
    if (handle == null) return '';
    try {
      final t = native.traffic(handle);
      return t == null ? '' : describeTraffic(t.rx, t.tx);
    } on Object {
      return '';
    }
  }

  @override
  Future<DateTime?> latestHandshake(String name) async {
    _checkName(name);
    final handle = _handles[name];
    if (handle == null) return null;
    final seconds = native.latestHandshake(handle);
    if (seconds <= 0) return null;
    return DateTime.fromMillisecondsSinceEpoch(seconds * 1000);
  }

  @override
  Future<List<String>> installed() async => _handles.keys.toList();

  /// A name only ever comes from this app; anything else is refused so that it
  /// cannot be confused with something else.
  void _checkName(String name) {
    if (!_validName.hasMatch(name)) {
      throw TunnelException('Недопустимое имя туннеля: $name');
    }
  }
}
