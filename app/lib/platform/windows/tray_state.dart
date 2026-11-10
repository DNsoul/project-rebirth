import '../../core/controller.dart';
import '../../core/regions.dart' show regionLabel;

/// Which of the four icons the notification area shows.
enum TrayIcon {
  /// Grey: not connected.
  idle('assets/tray/idle.ico'),

  /// Green: connected and the server answers.
  connected('assets/tray/connected.ico'),

  /// Amber: something is going on, or the server has gone quiet.
  busy('assets/tray/busy.ico'),

  /// Red: the last attempt failed.
  error('assets/tray/error.ico');

  const TrayIcon(this.asset);

  final String asset;
}

/// Everything the notification area shows and offers for one status. It is
/// worked out here, apart from the part that talks to the system, so that what
/// the icon says can be checked without a screen.
class TrayState {
  const TrayState({
    required this.icon,
    required this.tooltip,
    required this.statusLine,
    required this.toggleLabel,
    required this.toggleEnabled,
  });

  final TrayIcon icon;

  /// What appears when the pointer rests on the icon. Windows cuts it at 127
  /// characters.
  final String tooltip;

  /// The first line of the menu: what the app is doing.
  final String statusLine;

  /// The menu item that connects or disconnects, and whether it can be pressed
  /// right now.
  final String toggleLabel;
  final bool toggleEnabled;

  @override
  bool operator ==(Object other) =>
      other is TrayState &&
      other.icon == icon &&
      other.tooltip == tooltip &&
      other.statusLine == statusLine &&
      other.toggleLabel == toggleLabel &&
      other.toggleEnabled == toggleEnabled;

  @override
  int get hashCode =>
      Object.hash(icon, tooltip, statusLine, toggleLabel, toggleEnabled);

  @override
  String toString() => 'TrayState($icon, "$statusLine")';
}

/// What the menu's connect/disconnect item does, which depends on what the app
/// is doing: it is also the "cancel" item while a connection is being made.
enum TrayAction { connect, disconnect, cancel, none }

TrayAction trayToggleAction(AppStatus s) {
  switch (s.phase) {
    case Phase.idle:
    case Phase.failed:
      return TrayAction.connect;
    case Phase.connected:
      return TrayAction.disconnect;
    case Phase.connecting:
    case Phase.switching:
      return TrayAction.cancel;
    case Phase.disconnecting:
      return TrayAction.none;
  }
}

/// The longest tooltip Windows shows, including its terminator.
const maxTooltip = 127;

String _fit(String s) =>
    s.length < maxTooltip ? s : '${s.substring(0, maxTooltip - 2)}…';

TrayState trayStateFor(AppStatus s) {
  final place = s.region.isEmpty ? '' : ' · ${regionLabel(s.region)}';

  switch (s.phase) {
    case Phase.idle:
      return const TrayState(
        icon: TrayIcon.idle,
        tooltip: 'Rebirth: отключено',
        statusLine: 'Отключено',
        toggleLabel: 'Подключить',
        toggleEnabled: true,
      );
    case Phase.connecting:
      return const TrayState(
        icon: TrayIcon.busy,
        tooltip: 'Rebirth: подключаюсь…',
        statusLine: 'Подключаюсь…',
        toggleLabel: 'Отменить',
        toggleEnabled: true,
      );
    case Phase.connected:
      final quiet = s.lostContact || s.recovering;
      final line = s.recovering
          ? 'Восстанавливаю связь…$place'
          : s.lostContact
          ? 'Нет ответа от сервера$place'
          : 'Подключено$place';
      return TrayState(
        icon: quiet ? TrayIcon.busy : TrayIcon.connected,
        tooltip: _fit('Rebirth: ${line[0].toLowerCase()}${line.substring(1)}'),
        statusLine: line,
        toggleLabel: 'Отключить',
        toggleEnabled: true,
      );
    case Phase.switching:
      return TrayState(
        icon: TrayIcon.busy,
        tooltip: _fit('Rebirth: меняю сервер…'),
        statusLine: 'Меняю сервер…',
        toggleLabel: 'Отменить переход',
        toggleEnabled: true,
      );
    case Phase.disconnecting:
      return const TrayState(
        icon: TrayIcon.busy,
        tooltip: 'Rebirth: отключаюсь…',
        statusLine: 'Отключаюсь…',
        toggleLabel: 'Отключаюсь…',
        toggleEnabled: false,
      );
    case Phase.failed:
      final why = s.message.isEmpty ? 'не получилось' : s.message;
      return TrayState(
        icon: TrayIcon.error,
        tooltip: _fit('Rebirth: $why'),
        statusLine: 'Не получилось',
        toggleLabel: 'Повторить',
        toggleEnabled: true,
      );
  }
}
