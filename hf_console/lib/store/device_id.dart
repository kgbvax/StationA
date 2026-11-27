/// The device id of this console: a random name made once and kept in the
/// SharedPreferences, like the other settings of the app. powerseq echoes
/// it in `/state.chain.requester`, so the console that tapped the 75AUTO
/// Start knows that the chain is its own. It is not a secret.
library;

import 'package:shared_preferences/shared_preferences.dart';

import 'auto_op.dart' show newAutoOpCmdId;

const consoleDeviceIdKey = 'console_device_id';

/// The device id, made and stored at the first call.
Future<String> consoleDeviceId() async {
  final prefs = await SharedPreferences.getInstance();
  final stored = prefs.getString(consoleDeviceIdKey);
  if (stored != null && stored.isNotEmpty) return stored;
  final id = 'hf-console-${newAutoOpCmdId()}';
  await prefs.setString(consoleDeviceIdKey, id);
  return id;
}
