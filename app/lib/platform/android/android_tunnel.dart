import '../../core/platform.dart';
import 'awg_native.dart';
import 'conf_plan.dart';
import 'vpn_bridge.dart';

/// Brings tunnels up on Android: the system opens a VPN interface for the app,
/// and the tunnel library (the same amneziawg-go the servers run) moves the
/// packets through it.
///
/// Android lets an app hold one VPN interface at a time. Opening a new one
/// replaces the old, so a move from one server to the next costs a short gap
/// instead of overlapping the two, and the old tunnel is cleaned up afterwards
/// the way it is on Windows.
class AndroidTunnelDriver implements TunnelDriver {
  AndroidTunnelDriver({
    required this.native,
    required this.bridge,
    this.log,
    this.session = 'Rebirth',
  });

  final AwgNative native;
  final VpnBridge bridge;
  final void Function(String line)? log;

  /// What the system calls the connection in its status bar and settings.
  final String session;

  /// The tunnels that are running, by name, with the library's handle for each.
  final Map<String, int> _handles = {};

  static final _validName = RegExp(r'^awgr-[a-z0-9-]{1,24}$');

  @override
  Future<String?> preflight() async {
    try {
      final v = native.version();
      log?.call('tunnel library: $v');
    } on Object catch (e) {
      return 'Не загрузилась библиотека туннеля: $e';
    }
    final bool granted;
    try {
      granted = await bridge.prepare();
    } on Object catch (e) {
      return 'Не удалось запросить разрешение на VPN: $e';
    }
    return granted
        ? null
        : 'Нужно разрешить приложению VPN-подключение: без этого оно не может работать.';
  }

  @override
  Future<void> install({required String name, required String conf}) async {
    _checkName(name);
    final plan = planFromConf(conf);

    // A tunnel of the same name is stopped first: it would otherwise go on
    // holding a descriptor to an interface that is about to be replaced.
    if (_handles.containsKey(name)) {
      await uninstall(name);
    }

    final int fd;
    try {
      fd = await bridge.establish(
        session: session,
        addresses: plan.addresses,
        routes: plan.routes,
        dns: plan.dns,
        mtu: plan.mtu,
      );
    } on Object catch (e) {
      throw TunnelException('Система не открыла VPN-интерфейс: $e');
    }

    final handle = native.turnOn(conf, fd);
    if (handle <= 0) {
      // The library closes the descriptor when it cannot use it, so only the
      // service is left to let go of.
      if (_handles.isEmpty) await bridge.release();
      throw TunnelException(
        'Не удалось запустить туннель: ${native.lastError()}',
      );
    }
    _handles[name] = handle;

    // Its sockets must not be sent into the tunnel they carry. With the routes
    // this app uses they would not be, but a configuration may route everything.
    for (final socket in [native.socketV4(handle), native.socketV6(handle)]) {
      if (socket >= 0 && !await bridge.protect(socket)) {
        log?.call('could not keep socket $socket out of the tunnel');
      }
    }
    log?.call('tunnel $name is up (handle $handle)');
  }

  @override
  Future<void> uninstall(String name) async {
    _checkName(name);
    final handle = _handles.remove(name);
    if (handle == null) return;
    native.turnOff(handle);
    if (_handles.isEmpty) {
      await bridge.release();
    }
    log?.call('tunnel $name is down');
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
