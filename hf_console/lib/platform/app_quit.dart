// app_quit.dart — the settings dialog's QUIT APP key (Android only).
//
// On the kiosk tablet the console is the persistent HOME activity under lock
// task, so a plain finish() would relaunch it at once. The native side
// (MainActivity, channel `hf_console/app`, method `quit`) therefore leaves
// kiosk mode first (stop lock task, KioskPolicy.disable) and then finishes and
// removes the task. Starting the app again re-applies kiosk mode in onCreate.
// Device ownership is not touched: that stays behind the corner escape hatch.

import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

const _channel = MethodChannel('hf_console/app');

/// True where the console offers QUIT APP. iOS forbids programmatic quit, the
/// web build has nothing to quit and desktop builds close by their window.
bool get appQuitSupported =>
    !kIsWeb && defaultTargetPlatform == TargetPlatform.android;

/// Leaves kiosk mode and closes the app. Falls back to [SystemNavigator.pop]
/// when the native channel is missing (an older native shell).
Future<void> quitApp() async {
  try {
    await _channel.invokeMethod<void>('quit');
  } on MissingPluginException {
    await SystemNavigator.pop();
  }
}
