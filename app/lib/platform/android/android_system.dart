import '../../core/platform.dart';
import 'vpn_bridge.dart';

/// On Android the name server is part of the VPN interface itself: the system
/// is told to ask it, and only it, when the interface is opened (see
/// AndroidTunnelDriver). There is no separate rule to put in place or take away.
class AndroidDnsGuard implements DnsGuard {
  @override
  Future<void> enable(String server) async {}

  @override
  Future<void> disable() async {}
}

/// The Android ID: stable for this device and this app's signing key, and
/// unchanged by reinstalling the app. The server never sees it, only a hash of
/// it with a salt.
class AndroidMachineId implements MachineIdSource {
  AndroidMachineId(this.bridge);

  final VpnBridge bridge;

  @override
  Future<String> machineId() async {
    final id = await bridge.androidId();
    if (id.isEmpty) {
      throw StateError('the Android ID is not readable');
    }
    return 'android:$id';
  }
}
