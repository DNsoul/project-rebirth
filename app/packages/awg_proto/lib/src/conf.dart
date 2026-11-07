import 'b64.dart';
import 'body.dart';

/// The client configuration file for one device: its own key and the address the
/// server gave it, with everything else taken from the published body.
///
/// This is the Dart twin of `fleet.ConfigBody.ClientConf` in Go and has to
/// produce the same text to the byte (spec/conf-vectors.json): a stock AmneziaWG
/// client refuses a file over one stray character, and the file is what the app
/// hands to the tunnel.
///
/// Booleans are written as 1 and 0 because amneziawg-go rejects "on" and "off".
/// There is no byte order mark and the line endings are `\n`.
///
/// [dns] and [allowedIps] replace the body's own. The only user is the
/// enrollment tunnel, which is allowed to reach the server's registration
/// service and nothing else, and sets no DNS.
String renderClientConf({
  required ConfigBody body,
  required List<int> privateKey,
  required String address,
  int keepalive = 25,
  List<String>? dns,
  List<String>? allowedIps,
}) {
  final b = StringBuffer();
  void kv(String k, String v) {
    if (v.isEmpty) return;
    b.writeln('$k = $v');
  }

  final p = body.params;
  b.writeln('[Interface]');
  kv('PrivateKey', encodeStd(privateKey));
  kv('Address', address);
  final dnsServers = dns ?? body.dns;
  if (dnsServers.isNotEmpty) kv('DNS', dnsServers.join(', '));
  kv('MTU', '${body.mtu == 0 ? 1280 : body.mtu}');
  kv('Jc', '${p.jc}');
  kv('Jmin', '${p.jmin}');
  kv('Jmax', '${p.jmax}');
  kv('S1', '${p.s1}');
  kv('S2', '${p.s2}');
  kv('S3', '${p.s3}');
  kv('S4', '${p.s4}');
  if (p.headerProtectionKey.any((x) => x != 0)) {
    kv('HeaderProtectionKey', encodeStd(p.headerProtectionKey));
  }
  kv('RandomTrailers', p.randomTrailers ? '1' : '0');
  kv('I1', p.i1);

  b.writeln();
  b.writeln('[Peer]');
  kv('PublicKey', encodeStd(body.serverPublicKey));
  kv('AllowedIPs', (allowedIps ?? body.allowedIps).join(', '));
  kv('Endpoint', body.endpoint);
  kv('PersistentKeepalive', '$keepalive');
  return b.toString();
}
