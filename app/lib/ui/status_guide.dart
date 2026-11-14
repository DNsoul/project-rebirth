import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../core/controller.dart' show AppStatus, ConnectStep, Phase;
import 'stage.dart';
import 'stage_badge.dart';
import 'theme.dart';

/// What each state of the button means, in words: the one place the app explains
/// itself. The screen stays wordless; a small "i" in the top corner opens this.
///
/// Each state is shown by the button itself, small and alive, beside its name and
/// a line or two on what is going on and what to do. The strips at the top and
/// bottom of the window, and the long press, come after.

/// One state of the button, as the guide shows it.
@immutable
class GuideEntry {
  const GuideEntry(this.title, this.text, this.status);

  final String title;
  final String text;

  /// What the small button beside it is drawn from.
  final AppStatus status;
}

/// Every state the button can be in, in the order a person meets them.
const guideEntries = <GuideEntry>[
  GuideEntry(
    'Выключено',
    'Кнопка тёмная, огня нет. VPN не работает, интернет идёт напрямую. '
        'Нажмите, чтобы подключиться.',
    AppStatus(Phase.idle),
  ),
  GuideEntry(
    'Подключение',
    'Огонь разгорается, по ободу едет кружок со значком этапа (они ниже). '
        'Нажатие отменяет подключение.',
    AppStatus(Phase.connecting, step: ConnectStep.tunnel),
  ),
  GuideEntry(
    'Ждём владельца',
    'Устройство новое, и владелец должен его одобрить. Внизу написан код из '
        'четырёх цифр: назовите его владельцу. Нажатие отменяет ожидание.',
    AppStatus(
      Phase.connecting,
      step: ConnectStep.server,
      approvalCode: '0000',
    ),
  ),
  GuideEntry(
    'Подключено',
    'Огонь горит ровно, обод целиком оранжевый. VPN работает. '
        'Нажмите, чтобы отключить.',
    AppStatus(Phase.connected),
  ),
  GuideEntry(
    'Переход на новый сервер',
    'Огонь вспыхивает, по ободу бежит искра. Серверы время от времени '
        'меняются, и приложение переходит на новый само: делать ничего не нужно.',
    AppStatus(Phase.switching),
  ),
  GuideEntry(
    'Связь возвращается',
    'После сна или смены сети (Wi-Fi, мобильный интернет) по ободу бежит '
        'дуга: приложение восстанавливает связь. Подождите немного.',
    AppStatus(Phase.connected, lostContact: true, recovering: true),
  ),
  GuideEntry(
    'Сервер молчит',
    'Огонь неровный, обод пунктиром: сервер давно не отвечает или пропала '
        'сеть. Приложение само пробует снова. Если это затянулось, отключитесь '
        'и подключитесь.',
    AppStatus(Phase.connected, lostContact: true),
  ),
  GuideEntry(
    'Отключение',
    'Огонь гаснет, искра бежит назад. Занимает пару секунд.',
    AppStatus(Phase.disconnecting),
  ),
  GuideEntry(
    'Ошибка',
    'Красный уголёк бьётся, вокруг стрелка. Кружок с крестиком стоит на том '
        'этапе, где сломалось, а внизу написано, что случилось. '
        'Нажмите, чтобы повторить.',
    AppStatus(Phase.failed, step: ConnectStep.server),
  ),
];

/// What each stage of making a connection is, under the sign the disc shows.
const guideSteps = <ConnectStep, (String, String)>{
  ConnectStep.settings: ('Настройки', 'узнаю, к какому серверу идти'),
  ConnectStep.tunnel: ('Туннель', 'готовлю его на этом устройстве'),
  ConnectStep.server: (
    'Сервер',
    'жду ответа; в первый раз устройство ещё и регистрируется',
  ),
  ConnectStep.traffic: ('Трафик', 'проверяю, что данные проходят'),
};

/// Opens the guide. [canSaveLogs] adds a line about the long press, which only
/// does anything where saving the journal is possible.
Future<void> showStatusGuide(BuildContext context, {bool canSaveLogs = false}) =>
    showDialog<void>(
      context: context,
      barrierColor: const Color(0xB3000000),
      builder: (context) => StatusGuide(canSaveLogs: canSaveLogs),
    );

/// The guide itself, as a dialog.
class StatusGuide extends StatelessWidget {
  const StatusGuide({super.key, this.canSaveLogs = false});

  final bool canSaveLogs;

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    return Dialog(
      key: const ValueKey('status-guide'),
      backgroundColor: Ember.surface,
      surfaceTintColor: Colors.transparent,
      elevation: 0,
      insetPadding: const EdgeInsets.all(16),
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(16),
        side: const BorderSide(color: Ember.outline),
      ),
      clipBehavior: Clip.antiAlias,
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 440),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Padding(
              padding: const EdgeInsets.fromLTRB(20, 8, 6, 4),
              child: Row(
                children: [
                  Expanded(
                    child: Text(
                      'Что показывает кнопка',
                      style: text.titleMedium?.copyWith(
                        fontSize: 17,
                        fontWeight: FontWeight.w600,
                        color: Ember.text,
                      ),
                    ),
                  ),
                  IconButton(
                    key: const ValueKey('status-guide-close'),
                    tooltip: 'Закрыть',
                    onPressed: () => Navigator.of(context).pop(),
                    color: Ember.textDim,
                    icon: const Icon(Icons.close_rounded, size: 22),
                  ),
                ],
              ),
            ),
            const Divider(height: 1, thickness: 1, color: Ember.outline),
            Flexible(
              child: SingleChildScrollView(
                padding: const EdgeInsets.fromLTRB(16, 10, 20, 18),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    for (final e in guideEntries) ...[
                      _StateRow(entry: e),
                      if (e.status.phase == Phase.connecting &&
                          e.status.approvalCode.isEmpty)
                        const _Steps(),
                    ],
                    const SizedBox(height: 14),
                    const _Heading('Полосы сверху и снизу'),
                    const _StripRow(
                      colour: Ember.gold,
                      background: Ember.surfaceHigh,
                      text: 'Сверху: вышла новая версия. Нажмите на полосу, '
                          'чтобы обновить.',
                    ),
                    const _StripRow(
                      colour: Ember.alarm,
                      background: Color(0xFF241210),
                      text: 'Снизу красным: что пошло не так и что делать.',
                    ),
                    const _StripRow(
                      colour: Ember.amber,
                      background: Color(0xFF241B0E),
                      text: 'Снизу жёлтым: предупреждение. Приложение '
                          'работает, но что-то не так.',
                    ),
                    if (canSaveLogs) ...[
                      const SizedBox(height: 14),
                      const _Heading('Журнал'),
                      Padding(
                        padding: const EdgeInsets.only(left: 4),
                        child: Text(
                          'Долгое нажатие на кнопку сохраняет журнал в '
                          '«Загрузки». Его можно отправить владельцу, если '
                          'что-то не работает.',
                          style: _body(context),
                        ),
                      ),
                    ],
                  ],
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

TextStyle? _body(BuildContext context) => Theme.of(context).textTheme.bodySmall
    ?.copyWith(fontSize: 13, height: 1.35, color: Ember.textDim);

class _Heading extends StatelessWidget {
  const _Heading(this.text);

  final String text;

  @override
  Widget build(BuildContext context) => Padding(
    padding: const EdgeInsets.only(left: 4, bottom: 8),
    child: Text(
      text,
      style: Theme.of(context).textTheme.labelMedium?.copyWith(
        fontSize: 12,
        fontWeight: FontWeight.w600,
        letterSpacing: 0.4,
        color: Ember.gold,
      ),
    ),
  );
}

/// The button in one state, small, beside what that state means.
class _StateRow extends StatelessWidget {
  const _StateRow({required this.entry});

  final GuideEntry entry;

  /// The square the small button sits in, and the button. The light of the fire
  /// reaches 1.64 radii out, so the square has room for it.
  static const box = 64.0;
  static const button = 36.0;

  @override
  Widget build(BuildContext context) {
    final s = entry.status;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          SizedBox(
            width: box,
            height: box,
            child: ExcludeSemantics(
              child: IgnorePointer(
                child: RepaintBoundary(
                  child: Center(
                    child: RebirthStage(
                      phase: s.phase,
                      lostContact: s.lostContact,
                      recovering: s.recovering,
                      step: s.step,
                      size: button,
                      onPressed: _nothing,
                    ),
                  ),
                ),
              ),
            ),
          ),
          const SizedBox(width: 12),
          Expanded(
            child: Padding(
              padding: const EdgeInsets.only(top: 8),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    entry.title,
                    style: Theme.of(context).textTheme.bodyMedium?.copyWith(
                      fontSize: 14,
                      fontWeight: FontWeight.w600,
                      color: Ember.text,
                    ),
                  ),
                  const SizedBox(height: 2),
                  Text(entry.text, style: _body(context)),
                ],
              ),
            ),
          ),
        ],
      ),
    );
  }
}

void _nothing() {}

/// The four stages of a connection, each with the sign the disc shows for it.
class _Steps extends StatelessWidget {
  const _Steps();

  @override
  Widget build(BuildContext context) {
    final body = _body(context);
    return Padding(
      padding: const EdgeInsets.fromLTRB(_StateRow.box + 12, 2, 0, 8),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          for (final step in ConnectStep.values)
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 3),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Container(
                    width: 22,
                    height: 22,
                    decoration: const BoxDecoration(
                      shape: BoxShape.circle,
                      color: Ember.amber,
                    ),
                    child: Icon(
                      stepIcon(step),
                      size: 14,
                      color: Ember.surface,
                    ),
                  ),
                  const SizedBox(width: 10),
                  Expanded(
                    child: Padding(
                      padding: const EdgeInsets.only(top: 2),
                      child: Text.rich(
                        TextSpan(
                          children: [
                            TextSpan(
                              text: '${guideSteps[step]!.$1}: ',
                              style: const TextStyle(color: Ember.text),
                            ),
                            TextSpan(text: guideSteps[step]!.$2),
                          ],
                        ),
                        style: body,
                      ),
                    ),
                  ),
                ],
              ),
            ),
        ],
      ),
    );
  }
}

/// One of the strips at the edges of the window, drawn small, and what it says.
class _StripRow extends StatelessWidget {
  const _StripRow({
    required this.colour,
    required this.background,
    required this.text,
  });

  final Color colour;
  final Color background;
  final String text;

  @override
  Widget build(BuildContext context) => Padding(
    padding: const EdgeInsets.symmetric(vertical: 4),
    child: Row(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(4, 3, 0, 0),
          child: Container(
            width: _StateRow.box - 8,
            height: 14,
            padding: const EdgeInsets.symmetric(horizontal: 6),
            alignment: Alignment.centerLeft,
            decoration: BoxDecoration(
              color: background,
              borderRadius: BorderRadius.circular(3),
              border: Border.all(color: Ember.outline),
            ),
            // The line of words the strip carries, as a bar of its colour.
            child: FractionallySizedBox(
              widthFactor: 0.6,
              child: Container(
                height: 3,
                decoration: BoxDecoration(
                  color: colour,
                  borderRadius: BorderRadius.circular(2),
                ),
              ),
            ),
          ),
        ),
        const SizedBox(width: 16),
        Expanded(child: Text(text, style: _body(context))),
      ],
    ),
  );
}

/// The small "i" in the top corner that opens the guide. It is drawn rather than
/// written, so that the screen keeps to having no words on it.
class StatusGuideButton extends StatefulWidget {
  const StatusGuideButton({super.key, required this.onPressed});

  final VoidCallback onPressed;

  @override
  State<StatusGuideButton> createState() => _StatusGuideButtonState();
}

class _StatusGuideButtonState extends State<StatusGuideButton> {
  bool _hover = false;

  @override
  Widget build(BuildContext context) {
    return Semantics(
      button: true,
      label: 'Что значат состояния кнопки',
      child: Tooltip(
        message: 'Что значат состояния',
        waitDuration: const Duration(milliseconds: 500),
        excludeFromSemantics: true,
        child: MouseRegion(
          cursor: SystemMouseCursors.click,
          onEnter: (_) => setState(() => _hover = true),
          onExit: (_) => setState(() => _hover = false),
          child: Material(
            key: const ValueKey('status-guide-button'),
            color: Colors.transparent,
            shape: const CircleBorder(),
            child: InkWell(
              customBorder: const CircleBorder(),
              splashColor: Ember.orange.withValues(alpha: 0.14),
              highlightColor: Ember.orange.withValues(alpha: 0.06),
              onTap: widget.onPressed,
              child: SizedBox(
                width: 44,
                height: 44,
                child: Center(
                  child: TweenAnimationBuilder<Color?>(
                    tween: ColorTween(
                      end: _hover ? Ember.text : Ember.textDim,
                    ),
                    duration: const Duration(milliseconds: 160),
                    builder: (context, colour, _) => CustomPaint(
                      size: const Size.square(20),
                      painter: InfoMarkPainter(colour ?? Ember.textDim),
                    ),
                  ),
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }
}

/// A thin ring with a dot and a stem inside: the sign for "about this". Flat.
class InfoMarkPainter extends CustomPainter {
  const InfoMarkPainter(this.colour);

  final Color colour;

  @override
  void paint(Canvas canvas, Size size) {
    final s = math.min(size.width, size.height);
    final c = size.center(Offset.zero);
    final line = s * 0.085;
    final paint = Paint()
      ..color = colour
      ..style = PaintingStyle.stroke
      ..strokeWidth = line
      ..strokeCap = StrokeCap.round;
    canvas.drawCircle(c, s / 2 - line / 2, paint);
    // The stem, a little below the middle, and the dot above it.
    canvas.drawLine(
      c + Offset(0, -s * 0.04),
      c + Offset(0, s * 0.24),
      paint..strokeWidth = line * 1.15,
    );
    canvas.drawCircle(
      c + Offset(0, -s * 0.22),
      line * 0.8,
      Paint()..color = colour,
    );
  }

  @override
  bool shouldRepaint(InfoMarkPainter old) => old.colour != colour;
}
