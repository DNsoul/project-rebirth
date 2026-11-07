import '../../core/platform.dart';

/// What the system's VPN interface has to be told about a tunnel: the addresses
/// it has, the networks that go into it, the name servers it uses.
///
/// On Windows the installed client reads all this from the configuration file.
/// On Android the app does it itself, because the system takes these from the
/// app when it opens the interface, not from a file.
class TunnelPlan {
  const TunnelPlan({
    required this.addresses,
    required this.routes,
    required this.dns,
    required this.mtu,
  });

  final List<String> addresses;
  final List<String> routes;
  final List<String> dns;
  final int mtu;

  @override
  String toString() =>
      'TunnelPlan(addresses: $addresses, routes: $routes, dns: $dns, mtu: $mtu)';
}

/// Reads the interface and routing parts of a client configuration file.
///
/// The rest of the file (keys, the server, the obfuscation) is read by the
/// tunnel library; this takes only what the system needs.
TunnelPlan planFromConf(String conf) {
  var section = '';
  final addresses = <String>[];
  final routes = <String>[];
  final dns = <String>[];
  var mtu = 1280;

  List<String> list(String v) =>
      v.split(',').map((s) => s.trim()).where((s) => s.isNotEmpty).toList();
  String network(String s) => s.contains('/')
      ? s
      : s.contains(':')
      ? '$s/128'
      : '$s/32';

  for (final raw in conf.replaceAll('\r\n', '\n').split('\n')) {
    final line = raw.replaceFirst('\u{feff}', '').trim();
    if (line.isEmpty || line.startsWith('#')) continue;
    if (line.startsWith('[') && line.endsWith(']')) {
      section = line.substring(1, line.length - 1).toLowerCase();
      continue;
    }
    final eq = line.indexOf('=');
    if (eq < 0) continue;
    final key = line.substring(0, eq).trim().toLowerCase();
    final value = line.substring(eq + 1).trim();
    switch ('$section.$key') {
      case 'interface.address':
        addresses.addAll(list(value).map(network));
      case 'interface.dns':
        dns.addAll(list(value));
      case 'interface.mtu':
        mtu = int.tryParse(value) ?? mtu;
      case 'peer.allowedips':
        routes.addAll(list(value).map(network));
    }
  }

  if (addresses.isEmpty) {
    throw TunnelException('В настройках туннеля нет адреса.');
  }
  if (routes.isEmpty) {
    throw TunnelException(
      'В настройках туннеля нет сетей, которые идут в него.',
    );
  }
  return TunnelPlan(addresses: addresses, routes: routes, dns: dns, mtu: mtu);
}
