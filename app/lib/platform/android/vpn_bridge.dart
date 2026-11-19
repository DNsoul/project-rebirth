import 'package:flutter/services.dart';

import 'ru_apps.dart';

/// What only Android can do for the app: ask for the VPN permission, open the
/// system's VPN interface, keep sockets out of it, say which device this is. The
/// Kotlin side is MainActivity and AwgVpnService.
abstract class VpnBridge {
  /// Asks the user for permission to run a VPN if the app does not have it yet.
  /// True when it is granted.
  Future<bool> prepare();

  /// Opens the system's VPN interface and returns its file descriptor, which the
  /// caller owns. Replaces the interface that was open, if any. The apps named in
  /// [excluded] are left out of it: their traffic goes past the tunnel.
  Future<int> establish({
    required String session,
    required List<String> addresses,
    required List<String> routes,
    required List<String> dns,
    required int mtu,
    List<String> excluded = const [],
  });

  /// The apps installed on the phone that have something to start from the
  /// home screen, with the store each came from. The app itself is not among
  /// them.
  Future<List<InstalledApp>> installedApps();

  /// Keeps the socket with this descriptor out of the VPN. A socket that is not
  /// kept out would send its packets into the tunnel they are meant to carry.
  Future<bool> protect(int fd);

  /// Closes the VPN service, which takes the key off the status bar.
  Future<void> release();

  /// Changes what the notification of the running service says.
  Future<void> setStatus(String text);

  /// Makes sure the notification that shows what the VPN is doing can be seen:
  /// asks in a dialog on Android 13 and later, and opens the page of the app's
  /// settings (once) on phones that have it switched off. Answers `granted`,
  /// `denied`, or `settings` (sent to the settings page and come back). Returns
  /// when the person has answered.
  Future<String> ensureNotifications();

  /// Asks to be left running in the background: the system's own dialog "allow
  /// always running in the background?" (on Xiaomi phones their power page for the
  /// app). Answers `granted` or `denied` once the choice has taken effect.
  Future<String> ensureBatteryExemption();

  /// Asks for the right to install the updates the app downloads. Android has no
  /// dialog for it, so this opens the page "install unknown apps" for the app (not
  /// more than once a day) and answers `granted` or `denied` when the person is
  /// back.
  Future<String> ensureInstallPermission();

  /// This device's Android ID: it survives reinstalling the app.
  Future<String> androidId();

  /// What the phone calls itself: maker and model, e.g. "Xiaomi Redmi Note 12".
  Future<String> deviceModel();

  /// The app's private folder for files.
  Future<String> filesDir();

  /// Called when the system takes the VPN away from the app: the user withdrew
  /// the permission, or another VPN app took over.
  void Function()? onRevoked;

  /// Called when the network the phone reaches the internet by has changed (Wi-Fi
  /// to mobile data, a new Wi-Fi, a lost one). [available] is false when there is
  /// no usable network left at all. Only real changes are reported, not every
  /// flicker of the signal.
  void Function(bool available)? onNetworkChanged;

  /// Called when an app has been installed or removed.
  void Function()? onAppsChanged;
}

/// The real bridge, over a method channel.
class MethodChannelVpnBridge implements VpnBridge {
  MethodChannelVpnBridge([MethodChannel? channel])
    : _channel = channel ?? const MethodChannel(channelName) {
    _channel.setMethodCallHandler((call) async {
      if (call.method == 'revoked') {
        onRevoked?.call();
      } else if (call.method == 'networkChanged') {
        final args = call.arguments;
        final available = args is Map ? args['available'] != false : true;
        onNetworkChanged?.call(available);
      } else if (call.method == 'appsChanged') {
        onAppsChanged?.call();
      }
      return null;
    });
  }

  static const channelName = 'awg/vpn';

  final MethodChannel _channel;

  @override
  void Function()? onRevoked;

  @override
  void Function(bool available)? onNetworkChanged;

  @override
  void Function()? onAppsChanged;

  @override
  Future<bool> prepare() async =>
      await _channel.invokeMethod<bool>('prepare') ?? false;

  @override
  Future<int> establish({
    required String session,
    required List<String> addresses,
    required List<String> routes,
    required List<String> dns,
    required int mtu,
    List<String> excluded = const [],
  }) async {
    final fd = await _channel.invokeMethod<int>('establish', {
      'session': session,
      'addresses': addresses,
      'routes': routes,
      'dns': dns,
      'mtu': mtu,
      'excluded': excluded,
    });
    if (fd == null) {
      throw PlatformException(code: 'establish', message: 'no descriptor');
    }
    return fd;
  }

  @override
  Future<List<InstalledApp>> installedApps() async {
    final raw = await _channel.invokeListMethod<Object?>('installedApps') ?? [];
    return [
      for (final e in raw)
        if (e is Map && e['package'] is String)
          InstalledApp(
            e['package'] as String,
            installer: e['installer'] is String
                ? e['installer'] as String
                : null,
          ),
    ];
  }

  @override
  Future<bool> protect(int fd) async =>
      await _channel.invokeMethod<bool>('protect', {'fd': fd}) ?? false;

  @override
  Future<void> release() => _channel.invokeMethod<void>('release');

  @override
  Future<void> setStatus(String text) =>
      _channel.invokeMethod<void>('status', {'text': text});

  @override
  Future<String> ensureNotifications() async =>
      await _channel.invokeMethod<String>('notifications') ?? 'denied';

  @override
  Future<String> ensureBatteryExemption() async =>
      await _channel.invokeMethod<String>('battery') ?? 'denied';

  @override
  Future<String> ensureInstallPermission() async =>
      await _channel.invokeMethod<String>('install') ?? 'denied';

  @override
  Future<String> androidId() async =>
      await _channel.invokeMethod<String>('androidId') ?? '';

  @override
  Future<String> filesDir() async =>
      await _channel.invokeMethod<String>('filesDir') ?? '';

  @override
  Future<String> deviceModel() async =>
      await _channel.invokeMethod<String>('deviceModel') ?? '';
}
