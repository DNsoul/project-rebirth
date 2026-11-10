import 'dart:async';
import 'dart:math';
import 'dart:typed_data';

import 'package:awg_proto/awg_proto.dart';

import 'app_state.dart';
import 'bootstrap.dart';
import 'cancel.dart';
import 'enroll_client.dart';
import 'gist_source.dart';
import 'platform.dart';
import 'regions.dart';
import 'updater.dart' show ReleaseSource;

enum Phase { idle, connecting, connected, switching, disconnecting, failed }

/// The four stages of making a connection, in the order they come. The button
/// shows which one is being done (`stage.dart`); the words of each are in
/// [AppStatus.message].
enum ConnectStep {
  /// Looking at the machine and reading the settings.
  settings,

  /// Putting the tunnel in place.
  tunnel,

  /// Waiting for the server to answer (and, the first time, registering).
  server,

  /// Checking that traffic goes through (and turning on the protection of names).
  traffic,
}

/// What the app tells the user.
class AppStatus {
  const AppStatus(
    this.phase, {
    this.message = '',
    this.region = '',
    this.since,
    this.lostContact = false,
    this.note = '',
    this.step,
    this.approvalCode = '',
  });

  final Phase phase;

  /// While connecting, set when the server waits for its owner to let this
  /// device in: the four digits the owner sees next to the request.
  final String approvalCode;

  /// While connecting, the furthest stage reached; after a failure, the one it
  /// failed at. It never goes back within one attempt. Null otherwise.
  final ConnectStep? step;

  /// What is happening, or what went wrong, in words for the user.
  final String message;

  final String region;

  /// When the current connection came up.
  final DateTime? since;

  /// The tunnel is up but the server has stopped answering.
  final bool lostContact;

  /// Something worth saying that is not an error, e.g. that a move to a new
  /// server did not work out and the old one is still in use.
  final String note;

  bool get isUp => phase == Phase.connected || phase == Phase.switching;

  /// Connecting or moving: something is in progress that can still be called off.
  bool get canBeCancelled =>
      phase == Phase.connecting || phase == Phase.switching;
}

/// What the screen needs: what is going on, and what a person can ask for. The
/// screen depends on this and not on the controller, so it can be shown in any
/// state without arranging for one.
abstract class ConnectionView {
  AppStatus get status;
  Stream<AppStatus> get statusStream;
  Future<void> connect();
  Future<void> disconnect();

  /// Calls off a connection or a move that is still being made.
  Future<void> cancel();
}

/// A failure with a message that is fit to show.
class ConnectException implements Exception {
  ConnectException(this.message);

  final String message;

  @override
  String toString() => message;
}

class _Active {
  _Active({
    required this.slot,
    required this.body,
    required this.address,
    required this.since,
  });

  final String slot;
  final ConfigBody body;
  final String address;
  final DateTime since;
}

/// Brings the tunnel up, keeps it on the right server, and moves it when the
/// server does.
///
/// Everything it needs from the machine is passed in, so this is the same code
/// on Windows and Android, and the tests drive it with fakes and a clock they
/// control.
///
/// Two rules keep it responsive. The screen is told what is happening before
/// anything slow starts, and says which step it is on. And every operation can be
/// stopped: pressing "disconnect" or "cancel" tells the one in progress to stop
/// at its next step instead of waiting for it to finish.
class ConnectionController implements ConnectionView, ReleaseSource {
  ConnectionController({
    required this.bootstrap,
    required this.gist,
    required this.store,
    required this.tunnel,
    required this.dns,
    required this.probe,
    required this.machine,
    required this.transportFactory,
    this.deviceName = '',
    DateTime Function()? now,
    Future<void> Function(Duration)? sleep,
    this.pollEvery = const Duration(minutes: 3),
    this.healthEvery = const Duration(seconds: 10),
    this.updateEvery = const Duration(hours: 6),
    this.runTimers = true,
    this.networkAnswerWithin = const Duration(seconds: 15),
    Stream<bool>? networkChanges,
    void Function(String line)? log,
  }) : _now = now ?? DateTime.now,
       _sleep = sleep ?? ((Duration d) => Future<void>.delayed(d)),
       _log = log ?? ((_) {}) {
    _networkSub = networkChanges?.listen(_networkChanged);
  }

  final Bootstrap bootstrap;
  final GistSource gist;
  final StateStore store;
  final TunnelDriver tunnel;
  final DnsGuard dns;
  final DataPathProbe probe;
  final MachineIdSource machine;
  final EnrollTransportFactory transportFactory;

  /// A label for this device that the server shows its operator.
  final String deviceName;

  final Duration pollEvery;
  final Duration healthEvery;

  /// How often to look for a new version while the app stays open. Connected, the
  /// regular poll sees announcements too; this is for the time when it is not.
  final Duration updateEvery;

  /// Off in tests, which call [pollNow] and [checkHealthNow] themselves.
  final bool runTimers;

  /// After the network changes, how long the tunnel is given to reach the server
  /// on the new path before it is brought up again from scratch.
  final Duration networkAnswerWithin;

  final DateTime Function() _now;
  final Future<void> Function(Duration) _sleep;
  final void Function(String) _log;

  /// The tunnels this app owns. A connection alternates between the first two
  /// so that a new server can be brought up before the old one is let go.
  static const slots = ['awgr-a', 'awgr-b'];
  static const enrollSlot = 'awgr-enroll';
  static const tunnelPrefix = 'awgr-';

  final _statusController = StreamController<AppStatus>.broadcast();
  AppStatus _status = const AppStatus(Phase.idle);
  PersistedState? _state;
  _Active? _active;
  Timer? _pollTimer;
  Timer? _healthTimer;
  Timer? _updateTimer;
  Release? _release;
  final _releaseController = StreamController<Release>.broadcast();
  DateTime? _lastRefresh;
  DateTime? _retryNewServerAfter;
  Future<void> _lock = Future<void>.value();
  StreamSubscription<bool>? _networkSub;
  DateTime? _lastHeal;
  bool _networkRunning = false;
  bool _networkAgain = false;
  bool _networkAvailable = true;

  /// Belongs to the operation that is running; replaced when the next one starts.
  CancelToken _token = CancelToken();

  /// Set from the moment "disconnect" is pressed (or the app is leaving) until it
  /// is done, so that nothing queued behind it starts a connection of its own.
  bool _stopping = false;

  /// Whether the machine's name-resolution rule may be ours. Taking it away costs
  /// a second of PowerShell, so it is only done when there is something to take
  /// away: unknown at the start (a crashed run may have left one), then known.
  bool _dnsMayBeOn = true;

  @override
  AppStatus get status => _status;

  @override
  Stream<AppStatus> get statusStream => _statusController.stream;

  void _emit(AppStatus s) {
    _status = s;
    if (!_statusController.isClosed) {
      _statusController.add(s);
    }
  }

  /// Says which step is being taken, without changing anything else the screen
  /// shows. The person watching is told what the app is doing, not left with a
  /// spinner.
  ///
  /// [step], when given, is the stage this message belongs to. The stage only
  /// moves forward: a second try at the tunnel after a registration is still the
  /// stage the first one reached.
  void _progress(String message, [ConnectStep? step]) {
    final s = _status;
    if (s.phase == Phase.idle || s.phase == Phase.failed) {
      return;
    }
    final before = s.step;
    final reached = s.phase != Phase.connecting
        ? null
        : (step != null && (before == null || step.index > before.index)
              ? step
              : before);
    _emit(
      AppStatus(
        s.phase,
        message: message,
        region: s.region,
        since: s.since,
        lostContact: s.lostContact,
        note: s.note,
        step: reached,
      ),
    );
  }

  /// Says that the server waits for its owner, with the code to match. The next
  /// step said clears it, which is how it goes away once the owner says yes.
  void _waitingForOwner(String code) {
    final s = _status;
    if (s.phase != Phase.connecting) {
      return;
    }
    _emit(
      AppStatus(
        s.phase,
        message: 'Ждём одобрения владельца. Код для сверки: $code',
        region: s.region,
        since: s.since,
        note: s.note,
        step: s.step,
        approvalCode: code,
      ),
    );
  }

  /// Logs how long each step of an operation took. A slow step then shows up in
  /// the log with its name instead of as "it feels sluggish".
  void Function(String step) _lap(String operation) {
    final sw = Stopwatch()..start();
    var last = 0;
    return (step) {
      final now = sw.elapsedMilliseconds;
      _log('$operation: $step ${now - last} ms');
      last = now;
    };
  }

  // ---------------------------------------------------------------- lifecycle

  /// Call once when the app starts. Clears what a previous run may have left
  /// behind: a tunnel, or a resolver rule that would send every lookup to a
  /// server that is no longer there.
  ///
  /// It does not hold the window up: the screen is shown first and this runs
  /// behind it. A connection requested meanwhile simply waits its turn.
  Future<void> start() {
    final started = _exclusive('start', () async {
      await _restoreRelease(await _loadState());
      final problem = await tunnel.preflight();
      // Without the rights to run tunnels there is nothing this could have left
      // behind, and nothing it could clean up. And a cleanup that fails must not
      // stop the app from opening.
      if (problem == null) {
        await _teardownQuietly();
      }
      _emit(AppStatus(Phase.idle, note: problem ?? ''));
    });
    // The first look for a new version waits its turn behind the start-up work.
    if (runTimers) {
      _updateTimer = Timer.periodic(
        updateEvery,
        (_) => unawaited(checkForUpdate()),
      );
      unawaited(checkForUpdate());
    }
    return started;
  }

  @override
  Future<void> connect() {
    final phase = _status.phase;
    if (phase != Phase.idle && phase != Phase.failed) {
      return Future<void>.value();
    }
    // The screen answers before anything slow starts.
    _emit(
      const AppStatus(
        Phase.connecting,
        message: 'Готовлюсь…',
        step: ConnectStep.settings,
      ),
    );
    return _exclusive('connect', () async {
      if (_stopping) {
        return;
      }
      final token = _token;
      final lap = _lap('connect');
      // Whether anything has been put in place that a failure has to take back.
      // A machine that cannot run tunnels at all has nothing to clean up, and
      // is told so without waiting for a clean-up that has nothing to do.
      var started = false;
      try {
        final problem = await tunnel.preflight();
        if (problem != null) {
          throw ConnectException(problem);
        }
        started = true;
        lap('check of the system');
        token.check();

        await _removeStale();
        lap('removing leftovers');
        token.check();

        _progress('Читаю настройки…', ConnectStep.settings);
        final body = await _fetchBody(force: true, token: token);
        lap('settings');

        _active = await _bringUp(body, previous: null, token: token);
        lap('tunnel');
        _emit(
          AppStatus(
            Phase.connected,
            region: _active!.body.region,
            since: _active!.since,
          ),
        );
        _startTimers();
      } on Cancelled {
        _log('connect: cancelled');
        await _teardownQuietly();
        if (!_stopping) {
          _emit(const AppStatus(Phase.idle));
        }
      } catch (e) {
        _log('connect failed: $e');
        // Where it got to, so that the button can show where it broke.
        final brokeAt = _status.step;
        if (started) {
          // Undoing what was started takes a moment; the screen says so rather
          // than keeping "connecting" in front of someone who already has the
          // answer coming.
          _progress('Убираю за собой…');
          await _teardownQuietly();
        }
        _emit(AppStatus(Phase.failed, message: explain(e), step: brokeAt));
      }
    });
  }

  @override
  Future<void> disconnect() {
    // Whatever is running is told to stop, and the screen says so at once.
    _stopping = true;
    _token.cancel();
    _stopTimers();
    _emit(
      AppStatus(
        Phase.disconnecting,
        message: 'Отключаю туннель…',
        region: _status.region,
        since: _status.since,
      ),
    );
    return _exclusive('disconnect', () async {
      try {
        final lap = _lap('disconnect');
        await _teardownQuietly();
        lap('tunnel and names');
        _active = null;
      } finally {
        _stopping = false;
      }
      _emit(const AppStatus(Phase.idle));
    });
  }

  @override
  Future<void> cancel() async {
    if (_status.canBeCancelled) {
      _log('cancel requested');
      _progress('Отменяю…');
      _token.cancel();
    }
  }

  /// Takes the tunnel down for good. Called when the app exits.
  Future<void> dispose() async {
    _stopping = true;
    _token.cancel();
    _stopTimers();
    await _networkSub?.cancel();
    _networkSub = null;
    _updateTimer?.cancel();
    _updateTimer = null;
    await _exclusive('dispose', () async {
      await _teardownQuietly();
      _active = null;
    });
    await _statusController.close();
    await _releaseController.close();
  }

  // ------------------------------------------------------------------ timers

  void _startTimers() {
    if (!runTimers) {
      return;
    }
    _stopTimers();
    _pollTimer = Timer.periodic(pollEvery, (_) => unawaited(pollNow()));
    _healthTimer = Timer.periodic(
      healthEvery,
      (_) => unawaited(checkHealthNow()),
    );
  }

  void _stopTimers() {
    _pollTimer?.cancel();
    _healthTimer?.cancel();
    _pollTimer = _healthTimer = null;
  }

  /// Looks for a new server and moves to it if there is one.
  Future<void> pollNow() => _exclusive('poll', () async {
    if (_stopping) {
      return;
    }
    final active = _active;
    if (active == null || !_status.isUp) {
      return;
    }
    final token = _token;
    _lastRefresh = _now();

    final ConfigBody body;
    try {
      body = await _fetchBody(force: false, token: token);
    } on Cancelled {
      return;
    } catch (e) {
      _log('poll: could not read the configuration: $e');
      return;
    }
    if (!_isDifferentServer(body, active.body)) {
      return;
    }

    final retryAfter = _retryNewServerAfter;
    if (retryAfter != null && _now().isBefore(retryAfter)) {
      return;
    }

    _log('a new server is published: ${body.region} ${body.endpoint}');
    _emit(
      AppStatus(
        Phase.switching,
        message: 'Перехожу на новый сервер: ${regionLabel(body.region)}…',
        region: active.body.region,
        since: active.since,
      ),
    );
    try {
      final next = await _bringUp(body, previous: active, token: token);
      _active = next;
      _retryNewServerAfter = null;
      _emit(
        AppStatus(Phase.connected, region: next.body.region, since: next.since),
      );
    } on Cancelled {
      // Whoever cancelled has either asked to disconnect, which is handled
      // there, or to stay where it is.
      _log('switch: cancelled');
      if (!_stopping) {
        _emit(
          AppStatus(
            Phase.connected,
            region: active.body.region,
            since: active.since,
            note: 'Переход на новый сервер отменён. Остаёмся на прежнем.',
          ),
        );
      }
    } catch (e) {
      // Refused by the new server: the owner cut this device off, and the old
      // server has the same refusal. A new server that only wants the owner's
      // word is different: the old one still carries the traffic, so the app
      // stays on it and asks again later, like for a server that does not answer.
      if (e is EnrollRefused && e.reason == enrollDenied) {
        await _endShutOut(e);
        return;
      }
      // The old server is still there and still carrying the traffic.
      _log('switch failed: $e');
      _retryNewServerAfter = _now().add(const Duration(minutes: 1));
      _emit(
        AppStatus(
          Phase.connected,
          region: active.body.region,
          since: active.since,
          note:
              'Новый сервер пока не отвечает, остаёмся на прежнем. Попробуем снова через минуту.',
        ),
      );
    }
  });

  /// Looks at whether the server is still answering. If it has stopped, the
  /// configuration is read again at once, because the likeliest reason is that
  /// the server has been replaced.
  Future<void> checkHealthNow() async {
    final active = _active;
    if (_stopping ||
        active == null ||
        !_status.isUp ||
        _status.phase == Phase.switching) {
      return;
    }

    final DateTime? hs;
    try {
      hs = await tunnel.latestHandshake(active.slot);
    } catch (_) {
      return;
    }
    // WireGuard renews the handshake every two minutes while there is traffic,
    // and the keepalive makes sure there is. Three minutes is one missed round.
    final lost =
        !_networkAvailable ||
        hs == null ||
        _now().difference(hs) > const Duration(minutes: 3);
    if (lost != _status.lostContact && _status.phase == Phase.connected) {
      _emit(
        AppStatus(
          _status.phase,
          region: _status.region,
          since: _status.since,
          lostContact: lost,
          note: _status.note,
        ),
      );
    }
    if (lost && _networkAvailable) {
      final last = _lastRefresh;
      if (last == null ||
          _now().difference(last) > const Duration(seconds: 30)) {
        await pollNow();
      }
      // The server has not been heard from for minutes and the network is there,
      // so the cause is not a new server: whatever it is, the tunnel is dead, and
      // the way back is the one a person takes (off and on). Not more than once a
      // minute and a half, so a server that really is down is not hammered.
      //
      // Only an old handshake counts. One that could not be read at all proves
      // nothing: on Windows it is a program asked for the answer, and it may fail
      // for reasons of its own.
      final healed = _lastHeal;
      if (_status.lostContact &&
          hs != null &&
          !_networkRunning &&
          (healed == null ||
              _now().difference(healed) > const Duration(seconds: 90))) {
        _lastHeal = _now();
        _log('health: no contact for minutes, bringing the tunnel up again');
        unawaited(_networkChanged(true));
      }
    }
  }

  // ----------------------------------------------------------------- network

  /// The network the phone reaches the internet by has changed, or gone.
  ///
  /// The tunnel was made for the old path. Left to itself it waits for its own
  /// timers to notice, which takes a minute or more, and after a minute and a
  /// half of failed attempts it stops trying until something else is sent: a
  /// connection that does not come back until it is switched off and on.
  ///
  /// Changes that arrive while one is being dealt with are folded into a single
  /// further pass, so a burst of them (Wi-Fi lost, mobile data found, Wi-Fi back)
  /// is answered by where things ended up and not by every step on the way.
  Future<void> _networkChanged(bool available) async {
    _networkAvailable = available;
    if (_networkRunning) {
      _networkAgain = true;
      return;
    }
    _networkRunning = true;
    try {
      do {
        _networkAgain = false;
        await _afterNetworkChange();
      } while (_networkAgain && !_stopping);
    } finally {
      _networkRunning = false;
    }
  }

  Future<void> _afterNetworkChange() => _exclusive('network', () async {
    final active = _active;
    if (_stopping || active == null || _status.phase != Phase.connected) {
      return;
    }
    final token = _token;

    if (!_networkAvailable) {
      // Nothing to do but say so: the next change, when a network is back,
      // does the work.
      _log('network: none left');
      _setLostContact(true);
      return;
    }

    final since = _now();
    _log('network: changed, asking the tunnel to find the server again');
    try {
      await tunnel.rebind(active.slot);
    } on Object catch (e) {
      _log('network: the tunnel could not be nudged: $e');
    }

    try {
      if (await _waitHandshakeAfter(
        active.slot,
        since,
        networkAnswerWithin,
        token,
      )) {
        _log('network: the server answered on the new path');
        _setLostContact(false);
        return;
      }
      // The nudge was not enough: do what a person would, and bring the tunnel up
      // again. The old one stays until the new one works.
      _log(
        'network: no answer in ${networkAnswerWithin.inSeconds} s, '
        'bringing the tunnel up again',
      );
      _setLostContact(true);
      final next = await _bringUp(active.body, previous: active, token: token);
      _active = next;
      _emit(
        AppStatus(Phase.connected, region: next.body.region, since: next.since),
      );
    } on Cancelled {
      _log('network: cancelled');
    } catch (e) {
      if (_shutOut(e)) {
        await _endShutOut(e);
        return;
      }
      // The old tunnel is still there. The next change of network, or the health
      // check, tries again.
      _log('network: could not bring the tunnel up again: $e');
    }
  });

  /// Whether the server has said this device may not come in: the owner closed
  /// its access, or the server no longer knows it and wants the owner's word.
  /// Trying again on its own would get the same answer.
  bool _shutOut(Object e) =>
      e is EnrollRefused &&
      (e.reason == enrollDenied || e.reason == enrollPending);

  /// Takes the tunnel down and says why. It runs inside the operation that
  /// found out, so it does its own clearing up rather than queue another.
  Future<void> _endShutOut(Object e) async {
    _log('the server shut this device out: $e');
    _stopTimers();
    await _teardownQuietly();
    _active = null;
    _emit(AppStatus(Phase.failed, message: explain(e)));
  }

  void _setLostContact(bool lost) {
    final s = _status;
    if (s.phase != Phase.connected || s.lostContact == lost) {
      return;
    }
    _emit(
      AppStatus(
        s.phase,
        region: s.region,
        since: s.since,
        lostContact: lost,
        note: s.note,
      ),
    );
  }

  /// Waits for a handshake made at or after [since]. The library reports whole
  /// seconds, so one from the second before counts too.
  Future<bool> _waitHandshakeAfter(
    String slot,
    DateTime since,
    Duration within,
    CancelToken token,
  ) async {
    final deadline = _now().add(within);
    while (true) {
      token.check();
      final hs = await tunnel.latestHandshake(slot);
      if (hs != null && hs.add(const Duration(seconds: 1)).isAfter(since)) {
        return true;
      }
      if (!_now().isBefore(deadline)) {
        return false;
      }
      await token.guard(_sleep(const Duration(milliseconds: 300)));
    }
  }

  // ------------------------------------------------------------- configuration

  Future<PersistedState> _loadState() async => _state ??= await store.load();

  /// The current configuration: from the gist if it can be read and verified,
  /// otherwise the last one that was.
  Future<ConfigBody> _fetchBody({
    required bool force,
    required CancelToken token,
  }) async {
    final state = await _loadState();

    GistSnapshot? snapshot;
    String? gistProblem;
    try {
      snapshot = await token.guard(gist.fetch(etag: force ? null : state.etag));
      if (snapshot == null && state.configBody == null) {
        // "Unchanged", but there is nothing to be unchanged from.
        snapshot = await token.guard(gist.fetch());
      }
    } on GistException catch (e) {
      gistProblem = e.message;
    }
    token.check();

    if (snapshot != null) {
      final accepted = await _acceptAll(snapshot, state);
      if (accepted != null) {
        return accepted;
      }
      gistProblem ??=
          'Настройки сервера не прошли проверку подписи. Если так повторяется, установите свежую версию Rebirth.';
    }

    final cached = state.configBody;
    if (cached != null && state.configEpoch >= state.proto.minEpoch) {
      if (gistProblem != null) {
        _log('using the saved configuration: $gistProblem');
      }
      return ConfigBody.parse(cached);
    }
    throw ConnectException(
      gistProblem ??
          'Не удалось получить настройки. Проверьте интернет и повторите.',
    );
  }

  /// Verifies everything that was read, from every place the files are kept in,
  /// and makes the newest of it the current one. Which place had the newest does
  /// not matter, nor the order they are looked at in: a version older than one
  /// already accepted is refused by the checks that stop a replay, so whatever
  /// is newest is what is left.
  Future<ConfigBody?> _acceptAll(
    GistSnapshot snapshot,
    PersistedState state,
  ) async {
    final all = [snapshot, ...snapshot.others];

    // Every revocation first, from every place: one that is only in one of them
    // can still make the configuration in another unacceptable, so it has to be
    // known before any configuration is looked at.
    for (final one in all) {
      final r = one.revocation;
      if (r == null || r.isEmpty) {
        continue;
      }
      try {
        final (_, next) = await bootstrap.trust.openRevocation(
          r,
          _now(),
          state.proto,
        );
        state.proto = next;
        _log(
          'revocation: certificates below epoch ${next.minEpoch} are no longer accepted',
        );
      } on ProtoException {
        // The empty placeholder, one already applied, or something forged.
      }
    }

    var any = false;
    for (final one in all) {
      // With several places, one that is behind is the normal case, not
      // something to write down at every look.
      final body = await _accept(one, state, quietWhenOld: all.length > 1);
      any = any || body != null;
    }
    final current = state.configBody;
    if (!any || current == null) {
      return null;
    }
    // Remembered only once a configuration has been accepted, so that one that
    // fails its checks is read again next time instead of being taken for
    // unchanged.
    state.etag = snapshot.etag;
    await store.save(state);
    return ConfigBody.parse(current);
  }

  /// Verifies what was read and, if it holds up, makes it the current one.
  Future<ConfigBody?> _accept(
    GistSnapshot snapshot,
    PersistedState state, {
    bool quietWhenOld = false,
  }) async {
    final trust = bootstrap.trust;
    var proto = state.proto;

    // A revocation first: it can make the configuration next to it unacceptable.
    final r = snapshot.revocation;
    if (r != null && r.isNotEmpty) {
      try {
        final (_, next) = await trust.openRevocation(r, _now(), proto);
        proto = next;
        _log(
          'revocation: certificates below epoch ${proto.minEpoch} are no longer accepted',
        );
      } on ProtoException {
        // The empty placeholder, one already applied, or something forged.
      }
    }

    final c = snapshot.config;
    ConfigBody? body;
    if (c != null && c.isNotEmpty) {
      try {
        // The same publication again is fine: only an older one is refused.
        final (doc, _) = await trust.openDoc(
          c,
          kindConfig,
          _now(),
          proto.copyWith(lastSeq: max(0, proto.lastSeq - 1)),
        );
        body = ConfigBody.parse(doc.body);
        state
          ..proto = proto.copyWith(lastSeq: max(proto.lastSeq, doc.seq))
          ..configBody = doc.body
          ..configEpoch = doc.cert.epoch;
      } on ProtoException catch (e) {
        if (!quietWhenOld || !e.message.contains('is not newer')) {
          _log('the published configuration was refused: ${e.message}');
        }
      }
    }
    if (body == null) {
      state.proto = proto;
    }

    // Then the announcement of a new version, which is judged on its own: a
    // configuration that does not verify says nothing about it, and the other way
    // round.
    final u = snapshot.release;
    if (u != null && u.isNotEmpty) {
      try {
        final (rel, next) = await trust.openRelease(u, state.proto);
        state
          ..proto = next
          ..releaseBlob = u;
        _setRelease(rel);
      } on ProtoException catch (e) {
        if (!quietWhenOld || !e.message.contains('is older than')) {
          _log('the announced version was refused: ${e.message}');
        }
      }
    }
    await store.save(state);
    return body;
  }

  // ----------------------------------------------------------------- updates

  /// The newest version that has been announced, or null if none has. Whether it
  /// is newer than what is installed is for the caller to decide.
  @override
  Release? get release => _release;

  /// Fires when a different announcement has been accepted.
  @override
  Stream<Release> get releaseStream => _releaseController.stream;

  void _setRelease(Release r) {
    if (_release?.build == r.build && _release?.sig == r.sig) {
      return;
    }
    _release = r;
    if (!_releaseController.isClosed) {
      _releaseController.add(r);
    }
  }

  /// Reads back the announcement saved by an earlier run, verifying it again.
  Future<void> _restoreRelease(PersistedState state) async {
    final blob = state.releaseBlob;
    if (blob == null) {
      return;
    }
    try {
      final (rel, _) = await bootstrap.trust.openRelease(blob, state.proto);
      _setRelease(rel);
    } on ProtoException catch (e) {
      _log(
        'the saved announcement of a version no longer verifies: ${e.message}',
      );
      state.releaseBlob = null;
    }
  }

  /// Looks in the gist for an announcement of a new version, whether or not a
  /// connection is up. The configuration read on the way is kept like any other.
  Future<void> checkForUpdate() => _exclusive('update check', () async {
    if (_stopping) {
      return;
    }
    final state = await _loadState();
    try {
      final snapshot = await gist.fetch(etag: state.etag);
      if (snapshot != null) {
        await _acceptAll(snapshot, state);
      }
    } on GistException catch (e) {
      _log('update check: ${e.message}');
    }
  });

  bool _isDifferentServer(ConfigBody a, ConfigBody b) =>
      a.endpoint != b.endpoint ||
      encodeStd(a.serverPublicKey) != encodeStd(b.serverPublicKey);

  // ------------------------------------------------------------------ tunnel

  /// Brings the tunnel up against [body]. With [previous], the old tunnel is
  /// kept until the new one works and is removed only then. If [token] is
  /// cancelled, or anything fails, whatever was started here is taken down again.
  Future<_Active> _bringUp(
    ConfigBody body, {
    required _Active? previous,
    required CancelToken token,
  }) async {
    final state = await _loadState();
    final identity = await _identity();
    final serverKey = encodeStd(body.serverPublicKey);
    final slot = slots.firstWhere((s) => s != previous?.slot);
    final resolver = body.dns.isEmpty ? null : body.dns.first;

    // When moving, what is said starts with where to.
    final tag = previous == null ? '' : '${regionLabel(body.region)}: ';
    void say(String text, [ConnectStep? step]) => _progress('$tag$text', step);

    Future<bool> attempt(
      String address,
      Duration handshakeWithin,
      Duration answerWithin,
    ) async {
      final conf = renderClientConf(
        body: body,
        privateKey: identity.privateKey,
        address: address,
      );
      try {
        token.check();
        say('Поднимаю туннель…', ConnectStep.tunnel);
        await tunnel.install(name: slot, conf: conf);
        token.check();

        say('Жду ответа сервера…', ConnectStep.server);
        if (!await _waitHandshake(slot, handshakeWithin, token)) {
          _log(
            'no handshake with ${body.endpoint} within ${handshakeWithin.inSeconds}s',
          );
          say('Убираю туннель, который не заработал…');
          await tunnel.uninstall(slot);
          return false;
        }
        // The handshake shows the server knows the key; only an answer from the
        // resolver shows it also knows the address.
        if (resolver != null) {
          say('Проверяю, что трафик идёт…', ConnectStep.traffic);
          if (!await token.guard(
            probe.resolverAnswers(resolver, answerWithin),
          )) {
            _log('the tunnel is up but $resolver does not answer through it');
            say('Убираю туннель, который не заработал…');
            await tunnel.uninstall(slot);
            return false;
          }
        }
        return true;
      } on Object {
        // Cancelled, or something failed: do not leave the tunnel behind. A
        // cancelled attempt already says "cancelling"; anything else says it is
        // clearing up.
        if (!token.isCancelled) {
          say('Убираю за собой…');
        }
        await _quiet(() => tunnel.uninstall(slot));
        rethrow;
      }
    }

    // What the server has already confirmed for this device is tried first, then
    // what the last server called it: servers hand their devices on, so a new
    // one usually already knows this one.
    final known = state.addresses[serverKey];
    final guess = known ?? state.lastAddress;
    String? address;
    if (guess != null) {
      final ok = await attempt(
        guess,
        known != null
            ? const Duration(seconds: 20)
            : const Duration(seconds: 8),
        const Duration(seconds: 8),
      );
      if (ok) {
        address = guess;
      }
    }

    if (address == null) {
      say('Регистрирую это устройство…');
      // Only a connection the person is watching waits for the owner's approval:
      // they see the code and can cancel. A move or a repair in the background
      // that finds the device unknown gives up instead, and says why.
      final registered = await _register(
        body,
        identity,
        token,
        say,
        waitForApproval: previous == null,
      );
      _log('registered on ${body.region} ${body.endpoint} as $registered');
      if (!await attempt(
        registered,
        const Duration(seconds: 25),
        const Duration(seconds: 10),
      )) {
        throw ConnectException(
          'Сервер принял регистрацию, но не отвечает. Возможно, его уже заменили: повторите через минуту.',
        );
      }
      address = registered;
    }

    // It works. Now the machine's resolver, then the old tunnel.
    _log('$slot works as $address; resolver ${resolver ?? '-'} answers');
    token.check();
    try {
      if (resolver != null && _usesStandIns(resolver)) {
        say('Включаю защиту имён…', ConnectStep.traffic);
        _dnsMayBeOn = true;
        await dns.enable(resolver);
        _log('every name now goes to $resolver');
      } else if (_dnsMayBeOn) {
        await dns.disable();
        _dnsMayBeOn = false;
      }
      token.check();
    } on Object {
      // The new tunnel was never put to use; the old one, if any, still is.
      await _quiet(() => tunnel.uninstall(slot));
      rethrow;
    }
    if (previous != null) {
      await tunnel.uninstall(previous.slot);
    }

    state
      ..addresses[serverKey] = address
      ..lastAddress = address;
    await store.save(state);
    return _Active(slot: slot, body: body, address: address, since: _now());
  }

  /// The resolver that decides which sites go through the tunnel hands out
  /// addresses from the 198.18.0.0/15 block, and has to be the only one asked.
  bool _usesStandIns(String resolver) {
    final parts = resolver.split('.');
    if (parts.length != 4) {
      return false;
    }
    final a = int.tryParse(parts[0]);
    final b = int.tryParse(parts[1]);
    return a == 198 && (b == 18 || b == 19);
  }

  Future<bool> _waitHandshake(
    String slot,
    Duration within,
    CancelToken token,
  ) async {
    final deadline = _now().add(within);
    while (true) {
      token.check();
      final hs = await tunnel.latestHandshake(slot);
      if (hs != null) {
        return true;
      }
      if (!_now().isBefore(deadline)) {
        return false;
      }
      await token.guard(_sleep(const Duration(milliseconds: 300)));
    }
  }

  /// Registers this device through the enrollment peer: a tunnel that reaches
  /// the server's registration service and nothing else.
  Future<String> _register(
    ConfigBody body,
    _Identity identity,
    CancelToken token,
    void Function(String, [ConnectStep?]) say, {
    required bool waitForApproval,
  }) async {
    final info = body.enroll;
    if (info == null) {
      throw ConnectException(
        'Этот сервер старой версии и не принимает новые устройства. Повторите позже: серверы со временем заменяются.',
      );
    }
    final conf = renderClientConf(
      body: body,
      privateKey: info.privateKey,
      address: info.address,
      dns: const [],
      allowedIps: ['${_serverHost(info.server)}/32'],
      keepalive: 3,
    );
    final transport = transportFactory();
    try {
      token.check();
      // The same words as just before: only the stage moves on, to the tunnel.
      say('Регистрирую это устройство…', ConnectStep.tunnel);
      await tunnel.install(name: enrollSlot, conf: conf);
      token.check();
      // A service that is running is not yet a tunnel that carries anything: the
      // adapter still has to get its address and its route. The handshake is the
      // sign that the server has answered, and by then the machine is ready. It is
      // waited for, but not required: if it cannot be seen, the datagrams are
      // sent anyway, since they are what starts the handshake in the first place.
      say('Связываюсь с сервером…', ConnectStep.server);
      final shook = await _waitHandshake(
        enrollSlot,
        const Duration(seconds: 15),
        token,
      );
      _log('registration: tunnel up, handshake ${shook ? 'seen' : 'not seen'}');
      say('Регистрирую это устройство…', ConnectStep.server);
      // The same four digits stand next to the request in the owner's chat,
      // so the two can be matched over the phone.
      final code = await approvalCode(identity.publicKey);
      var told = false;
      return await registerDevice(
        transport: transport,
        contentKey: bootstrap.contentKey,
        deviceId: identity.id,
        publicKey: identity.publicKey,
        name: deviceName,
        now: _now,
        sleep: _sleep,
        cancel: token,
        onPending: !waitForApproval
            ? null
            : () {
                if (!told) {
                  told = true;
                  _log('registration: waiting for the owner, code $code');
                }
                _waitingForOwner(code);
              },
      );
    } on TimeoutException {
      _log('registration timed out: ${transport.describe()}');
      throw ConnectException(
        'Сервер не ответил на регистрацию. Проверьте интернет и повторите.',
      );
    } finally {
      await transport.close();
      await _quiet(() => tunnel.uninstall(enrollSlot));
    }
  }

  String _serverHost(String hostPort) {
    final i = hostPort.lastIndexOf(':');
    return i < 0 ? hostPort : hostPort.substring(0, i);
  }

  // ---------------------------------------------------------------- identity

  Future<_Identity> _identity() async {
    final state = await _loadState();
    final TunnelKeyPair pair;
    if (state.deviceKey == null) {
      pair = await generateTunnelKeyPair();
      state.deviceKey = encodeStd(pair.privateKey);
      await store.save(state);
    } else {
      pair = await tunnelKeyPairFromPrivate(
        decodeStd(state.deviceKey!, tunnelKeySize),
      );
    }
    final String machineId;
    try {
      machineId = await machine.machineId();
    } catch (_) {
      throw ConnectException(
        'Не удалось определить это устройство. Перезапустите Rebirth.',
      );
    }
    return _Identity(
      await deviceIdFrom(machineId),
      pair.privateKey,
      pair.publicKey,
    );
  }

  // ----------------------------------------------------------------- cleanup

  Future<void> _quiet(Future<void> Function() step) async {
    try {
      await step();
    } on Object catch (e) {
      _log('cleanup step failed: $e');
    }
  }

  Future<List<String>> _ourTunnels() async => [
    for (final name in await tunnel.installed())
      if (name.startsWith(tunnelPrefix)) name,
  ];

  /// Removes tunnels left from before, so that a connection starts clean. The
  /// resolver rule is only touched if it may be there.
  Future<void> _removeStale() async {
    final names = await _ourTunnels();
    await Future.wait([
      for (final name in names) tunnel.uninstall(name),
      if (_dnsMayBeOn) _giveBackNames(),
    ]);
  }

  /// Puts the machine's name resolution back, if it needs it.
  Future<void> _giveBackNames() async {
    if (!_dnsMayBeOn) {
      return;
    }
    await dns.disable();
    _dnsMayBeOn = false;
  }

  /// Removes every tunnel this app owns and puts the machine's resolution back.
  /// The two are independent and each takes a moment, so they go side by side;
  /// a failure in one does not stop the other.
  Future<void> _teardownQuietly() async {
    await Future.wait([
      _quiet(() async {
        for (final name in await _ourTunnels()) {
          await tunnel.uninstall(name);
        }
      }),
      _quiet(_giveBackNames),
    ]);
  }

  // -------------------------------------------------------------------- misc

  /// Runs [body] after the operations queued before it. Each gets a token of its
  /// own, and its duration is logged.
  Future<T> _exclusive<T>(String name, Future<T> Function() body) {
    final done = Completer<T>();
    _lock = _lock.then((_) async {
      _token = CancelToken();
      final sw = Stopwatch()..start();
      try {
        done.complete(await body());
      } catch (e, s) {
        done.completeError(e, s);
      }
      _log('$name: ${sw.elapsedMilliseconds} ms');
    });
    return done.future;
  }

  /// A failure in words for the user.
  static String explain(Object e) {
    if (e is ConnectException) {
      return e.message;
    }
    if (e is EnrollRefused) {
      return e.toString();
    }
    if (e is GistException) {
      return e.message;
    }
    if (e is ProtoException) {
      return 'Настройки не прошли проверку. Если так повторяется, установите свежую версию Rebirth. (${e.message})';
    }
    if (e is TimeoutException) {
      return 'Сервер не ответил вовремя. Повторите попытку.';
    }
    return 'Не удалось подключиться. Повторите попытку. ($e)';
  }
}

class _Identity {
  _Identity(this.id, this.privateKey, this.publicKey);

  final Uint8List id;
  final Uint8List privateKey;
  final Uint8List publicKey;
}
