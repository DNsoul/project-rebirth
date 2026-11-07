import 'dart:io';

import 'app_android.dart';
import 'app_windows.dart';

/// One program for both systems; what differs is in app_windows.dart and
/// app_android.dart. The logic of connecting, and the screen, are shared.
Future<void> main(List<String> args) async {
  if (Platform.isAndroid) {
    await runAndroid(args);
  } else {
    await runWindows(args);
  }
}
