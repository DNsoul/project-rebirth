import 'dart:ui';

import 'package:flutter/material.dart';

import 'core/controller.dart';
import 'core/updater.dart';
import 'ui/home_page.dart';
import 'ui/theme.dart';

class App extends StatefulWidget {
  const App({
    super.key,
    required this.controller,
    required this.hint,
    this.updates,
  });

  final ConnectionController controller;

  /// Offers a newer version, when there is one.
  final UpdateView? updates;

  /// What the screen says under the button about the app staying in the
  /// background, which differs between the systems.
  final String hint;

  @override
  State<App> createState() => _AppState();
}

class _AppState extends State<App> {
  late final AppLifecycleListener _lifecycle;

  @override
  void initState() {
    super.initState();
    // If the system ends the program some other way, the tunnel still comes down
    // and the machine still gets its names back: a rule that sends them to a
    // server nothing is looking after would leave it without a working internet
    // connection.
    _lifecycle = AppLifecycleListener(
      onExitRequested: () async {
        await widget.controller.dispose();
        return AppExitResponse.exit;
      },
    );
  }

  @override
  void dispose() {
    _lifecycle.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Rebirth',
      debugShowCheckedModeBanner: false,
      themeMode: ThemeMode.dark,
      theme: appTheme(Brightness.light),
      darkTheme: appTheme(Brightness.dark),
      home: HomePage(
        controller: widget.controller,
        hint: widget.hint,
        updates: widget.updates,
      ),
    );
  }
}

class BrokenApp extends StatelessWidget {
  const BrokenApp(this.message, {super.key});

  final String message;

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      home: Scaffold(
        body: Center(
          child: Padding(
            padding: const EdgeInsets.all(24),
            child: Text(message, textAlign: TextAlign.center),
          ),
        ),
      ),
    );
  }
}
