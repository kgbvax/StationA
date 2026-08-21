import 'dart:io';
import 'package:flutter/foundation.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:shared_preferences/shared_preferences.dart';

class CredentialStore {
  final _secure = const FlutterSecureStorage();

  bool get _useSecureStorage => !kIsWeb && Platform.isAndroid;

  Future<Map<String, String?>> readAll() async {
    if (_useSecureStorage) {
      return await _secure.readAll();
    }
    final prefs = await SharedPreferences.getInstance();
    return {
      'mqtt_host': prefs.getString('mqtt_host'),
      'mqtt_port': prefs.getString('mqtt_port'),
      'mqtt_user': prefs.getString('mqtt_user'),
      'mqtt_password': prefs.getString('mqtt_password'),
      // DX-spot overlay settings (the Android path returns these via _secure.readAll()
      // automatically; the non-secure SharedPreferences fallback lists them explicitly).
      'station_locator': prefs.getString('station_locator'),
      'horstreporter_base_url': prefs.getString('horstreporter_base_url'),
      'station_callsign': prefs.getString('station_callsign'),
    };
  }

  Future<void> writeAll(Map<String, String> values) async {
    if (_useSecureStorage) {
      for (final e in values.entries) {
        await _secure.write(key: e.key, value: e.value);
      }
      return;
    }
    final prefs = await SharedPreferences.getInstance();
    for (final e in values.entries) {
      await prefs.setString(e.key, e.value);
    }
  }
}
