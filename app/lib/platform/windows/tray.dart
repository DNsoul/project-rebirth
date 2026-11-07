import 'dart:async';

import 'package:flutter/services.dart';

import '../../core/controller.dart';
import 'tray_state.dart';

/// The icon in the notification area, as Dart sees it: it says what the icon
/// should show, and hears which menu item was chosen.
///
/// The icon itself, its menu, and what a click on it does (show the window, open
/// the menu) are in the runner's native code, on a thread of their own. Talking
/// to the notification area waits for Explorer, and Explorer is slow for seconds
/// while a network adapter appears or goes away, which is exactly when the VPN
/// connects or disconnects; done in-process it froze the window each time. Here
/// a call just hands the new state over and returns.
class NativeTray {
  NativeTray({
    required this.view,
    required this.onQuit,
    this.log,
    MethodChannel? channel,
  }) : _channel = channel ?? const MethodChannel(channelName);

  static const channelName = 'awg/tray';

  /// Labels for the two menu items that are always there. They are sent over so
  /// that the native side holds no text.
  static const showLabel = 'Открыть окно';
  static const quitLabel = 'Выход';

  final ConnectionView view;

  /// Ends the app, after taking the tunnel down.
  final Future<void> Function() onQuit;

  final void Function(String line)? log;
  final MethodChannel _channel;

  StreamSubscription<AppStatus>? _subscription;
  TrayState? _sent;
  AppStatus _status = const AppStatus(Phase.idle);

  Future<void> init() async {
    _channel.setMethodCallHandler(_fromNative);
    _status = view.status;
    _push(trayStateFor(_status));
    _subscription = view.statusStream.listen((s) {
      _status = s;
      _push(trayStateFor(s));
    });
  }

  void _push(TrayState next) {
    if (next == _sent) {
      return; // Explorer is only ever told about a change.
    }
    _sent = next;
    unawaited(
      _channel
          .invokeMethod<void>('update', {
            'icon': next.icon.index,
            'tooltip': next.tooltip,
            'status': next.statusLine,
            'toggleLabel': next.toggleLabel,
            'toggleEnabled': next.toggleEnabled,
            'showLabel': showLabel,
            'quitLabel': quitLabel,
          })
          .catchError((Object e) {
            // The icon is a convenience on top of a working app.
            log?.call('tray: $e');
          }),
    );
  }

  Future<Object?> _fromNative(MethodCall call) async {
    if (call.method != 'command') {
      return null;
    }
    switch (call.arguments) {
      case 'toggle':
        switch (trayToggleAction(_status)) {
          case TrayAction.connect:
            unawaited(view.connect());
          case TrayAction.disconnect:
            unawaited(view.disconnect());
          case TrayAction.cancel:
            unawaited(view.cancel());
          case TrayAction.none:
            break;
        }
      case 'quit':
        unawaited(onQuit());
    }
    return null;
  }

  /// Takes the icon out of the notification area. The native side waits a little
  /// for Explorer but not long, and so does this.
  Future<void> dispose() async {
    await _subscription?.cancel();
    _subscription = null;
    try {
      await _channel
          .invokeMethod<void>('remove')
          .timeout(const Duration(seconds: 3));
    } on Object catch (e) {
      log?.call('tray: $e');
    }
  }
}
