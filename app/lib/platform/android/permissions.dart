import 'vpn_bridge.dart';

/// Asks for everything the app needs from the person, once, when it starts, one
/// question at a time (two system dialogs at once would cover each other):
///
///  1. the notification that says what the VPN is doing. Without the right to show
///     it, it never appears: on Android 13 and later that is the default for a
///     program that has not asked, and on some phones (Xiaomi's) for every program
///     that did not come from the store;
///  2. to be left running in the background. A VPN that the system stops to save
///     battery when the screen goes off is no VPN;
///  3. to install the updates it downloads (a page of the settings: Android has no
///     dialog for it).
///
/// The VPN permission itself is asked right after this, by the controller's
/// start-up check (`AndroidTunnelDriver.preflight`).
///
/// A refusal is not an error: the app works without any of them. What is still
/// missing is asked for again at the next start, so that a person who went back by
/// mistake is not left without it (the pages of the settings not more than once a
/// day, so as not to be a nuisance). Whatever goes wrong with one is written to the
/// log and the next is asked all the same.
Future<void> askForPermissions(
  VpnBridge bridge, {
  void Function(String line)? log,
}) async {
  final questions = <(String, Future<String> Function())>[
    ('notifications', bridge.ensureNotifications),
    ('battery', bridge.ensureBatteryExemption),
    ('install', bridge.ensureInstallPermission),
  ];
  for (final (name, ask) in questions) {
    try {
      final answer = await ask();
      log?.call('permissions: $name $answer');
    } on Object catch (e) {
      log?.call('permissions: $name: $e');
    }
  }
}
