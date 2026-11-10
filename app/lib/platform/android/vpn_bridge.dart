import 'package:flutter/services.dart';

/// What only Android can do for the app: ask for the VPN permission, open the
/// system's VPN interface, keep sockets out of it, say which device this is. The
/// Kotlin side is MainActivity and AwgVpnService.
abstract class VpnBridge {
  /// Asks the user for permission to run a VPN if the app does not have it yet.
  /// True when it is granted.
  Future<bool> prepare();

  /// Opens the system's VPN interface and returns its file descriptor, which the
  /// caller owns. Replaces the interface that was open, if any.
  Future<int> establish({
    required String session,
    required List<String> addresses,
    required List<String> routes,
    required List<String> dns,
    required int mtu,
  });

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
  Future<bool> prepare() async =>
      await _channel.invokeMethod<bool>('prepare') ?? false;

  @override
  Future<int> establish({
    required String session,
    required List<String> addresses,
    required List<String> routes,
    required List<String> dns,
    required int mtu,
  }) async {
    final fd = await _channel.invokeMethod<int>('establish', {
      'session': session,
      'addresses': addresses,
      'routes': routes,
      'dns': dns,
      'mtu': mtu,
    });
    if (fd == null) {
      throw PlatformException(code: 'establish', message: 'no descriptor');
    }
    return fd;
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
}
