import 'dart:convert';
import 'dart:io';

import 'package:awg_proto/awg_proto.dart';

/// Everything the app remembers between runs.
class PersistedState {
  PersistedState({
    this.proto = const ProtoState(),
    this.configBody,
    this.configEpoch = 0,
    this.etag,
    this.releaseBlob,
    this.deviceKey,
    Map<String, String>? addresses,
    this.lastAddress,
  }) : addresses = addresses ?? {};

  factory PersistedState.fromJson(Map<String, Object?> j) {
    final p = j['proto'];
    final a = j['addresses'];
    return PersistedState(
      proto: p is Map
          ? ProtoState.fromJson(p.cast<String, Object?>())
          : const ProtoState(),
      configBody: j['config_body'] as String?,
      configEpoch: (j['config_epoch'] as int?) ?? 0,
      // An etag saved before announcements existed says "nothing new" about a
      // gist whose announcement was never read, so it is not trusted.
      etag: j['etag_has_release'] == true ? j['etag'] as String? : null,
      releaseBlob: j['release'] as String?,
      deviceKey: j['device_key'] as String?,
      addresses: a is Map
          ? a.map((k, v) => MapEntry(k as String, v as String))
          : null,
      lastAddress: j['last_address'] as String?,
    );
  }

  /// What the verifier remembers: the lowest certificate epoch it still accepts
  /// and the newest configuration it has taken.
  ProtoState proto;

  /// The text of the last configuration that passed verification. It is what
  /// the app falls back to when the gist cannot be read or serves something
  /// that does not verify.
  String? configBody;

  /// The certificate epoch that configuration was published under. If the
  /// operator later revokes below it, the cached configuration is not used
  /// either: it came from a server key that is no longer trusted.
  int configEpoch;

  /// For asking the gist "anything new?" without spending a request.
  String? etag;

  /// The sealed announcement of the newest version that passed verification.
  /// Kept as it came, so that it is verified again when read back and the state
  /// file is never trusted for more than the gist would be. It is what keeps the
  /// offer alive across restarts, and while the gist says "unchanged" and so does
  /// not send it again.
  String? releaseBlob;

  /// This device's own tunnel key, base64 as in a `.conf` file. Made once.
  String? deviceKey;

  /// The address each server has confirmed for this device, keyed by the
  /// server's public key.
  Map<String, String> addresses;

  /// The address of the last server that worked. A new server usually already
  /// knows the device under the same one, because servers hand their devices
  /// on, so it is tried before registering again.
  String? lastAddress;

  Map<String, Object?> toJson() => {
    'proto': proto.toJson(),
    if (configBody != null) 'config_body': configBody,
    'config_epoch': configEpoch,
    if (etag != null) 'etag': etag,
    if (etag != null) 'etag_has_release': true,
    if (releaseBlob != null) 'release': releaseBlob,
    if (deviceKey != null) 'device_key': deviceKey,
    'addresses': addresses,
    if (lastAddress != null) 'last_address': lastAddress,
  };
}

abstract class StateStore {
  Future<PersistedState> load();
  Future<void> save(PersistedState state);
}

/// Keeps nothing past the process. For tests.
class MemoryStateStore implements StateStore {
  MemoryStateStore([PersistedState? initial])
    : _json = initial == null ? null : jsonEncode(initial.toJson());

  String? _json;
  int saves = 0;

  @override
  Future<PersistedState> load() async => _json == null
      ? PersistedState()
      : PersistedState.fromJson(jsonDecode(_json!) as Map<String, Object?>);

  @override
  Future<void> save(PersistedState state) async {
    saves++;
    _json = jsonEncode(state.toJson());
  }
}

/// A JSON file, replaced as a whole on every save so that a crash in the middle
/// never leaves half of one.
class FileStateStore implements StateStore {
  FileStateStore(this.path);

  final String path;

  @override
  Future<PersistedState> load() async {
    final file = File(path);
    if (!await file.exists()) return PersistedState();
    try {
      final raw = jsonDecode(await file.readAsString());
      if (raw is Map) {
        return PersistedState.fromJson(raw.cast<String, Object?>());
      }
    } on FormatException {
      // Falls through: unreadable state is the same as none. The device just
      // registers again, which costs seconds, and a new key replaces the old.
    } on TypeError {
      // Same.
    }
    return PersistedState();
  }

  @override
  Future<void> save(PersistedState state) async {
    final file = File(path);
    await file.parent.create(recursive: true);
    final tmp = File('$path.tmp');
    await tmp.writeAsString(
      const JsonEncoder.withIndent('  ').convert(state.toJson()),
      flush: true,
    );
    await tmp.rename(path);
  }
}
