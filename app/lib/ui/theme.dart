import 'package:flutter/material.dart';

/// The colours of Rebirth: soot and embers. The background is the warm black of a
/// burnt-out fire, and everything alive is lit from within, gold at the crest and
/// orange through to red in the tail of the mark.
abstract final class Ember {
  /// Behind everything.
  static const background = Color(0xFF0F0B0A);

  /// Cards and discs; one step above the background.
  static const surface = Color(0xFF17110F);

  /// Raised parts: buttons at rest, the inside of the big button.
  static const surfaceHigh = Color(0xFF241A16);

  /// Lines and the ring of a button that is off.
  static const outline = Color(0xFF3B2D27);

  /// Text, and text that matters less.
  static const text = Color(0xFFF4EAE2);
  static const textDim = Color(0xFFA99A91);

  /// The flame, from the crest to the tail.
  static const gold = Color(0xFFFFD36B);
  static const orange = Color(0xFFFF8A3D);
  static const red = Color(0xFFE5322B);

  /// Something is going on, or the server has gone quiet.
  static const amber = Color(0xFFF2B134);

  /// A failure; lighter than [red] so that it reads on the dark ground.
  static const alarm = Color(0xFFFF6B5E);

  /// The mark when nothing is lit: cold ash.
  static const ash = Color(0xFF6B5E57);
}

/// The look of the app. It is dark whatever the system says: the mark is made to
/// be seen against soot, and a light version would be a different app.
ThemeData appTheme([Brightness brightness = Brightness.dark]) {
  const scheme = ColorScheme.dark(
    primary: Ember.orange,
    onPrimary: Color(0xFF2A1205),
    primaryContainer: Color(0xFF4A2410),
    onPrimaryContainer: Ember.gold,
    secondary: Ember.gold,
    onSecondary: Color(0xFF2A1F05),
    tertiary: Ember.amber,
    onTertiary: Color(0xFF2A1F05),
    tertiaryContainer: Color(0xFF3F2E0E),
    onTertiaryContainer: Ember.gold,
    error: Ember.alarm,
    onError: Color(0xFF330A07),
    errorContainer: Color(0xFF4A1512),
    onErrorContainer: Color(0xFFFFB4AB),
    surface: Ember.surface,
    onSurface: Ember.text,
    onSurfaceVariant: Ember.textDim,
    surfaceContainerHighest: Ember.surfaceHigh,
    outline: Ember.outline,
    outlineVariant: Ember.outline,
  );

  final base = ThemeData(
    useMaterial3: true,
    brightness: Brightness.dark,
    colorScheme: scheme,
    scaffoldBackgroundColor: Ember.background,
  );

  return base.copyWith(
    textTheme: base.textTheme.apply(
      bodyColor: Ember.text,
      displayColor: Ember.text,
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: Ember.surfaceHigh,
        foregroundColor: Ember.gold,
        side: const BorderSide(color: Ember.outline),
        shape: const StadiumBorder(),
        padding: const EdgeInsets.symmetric(horizontal: 24, vertical: 12),
      ),
    ),
    progressIndicatorTheme: const ProgressIndicatorThemeData(
      color: Ember.amber,
    ),
  );
}
