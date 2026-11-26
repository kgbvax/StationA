import 'dart:io';
import 'package:flutter/foundation.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'auto_op.dart' show parseAutoOpKey;

/// The storage key of the 75auto command key (64 hex digits, contract §7.2).
/// It is never in the APK. The operator enters it once in the gear sheet.
const autoOpKeyStorageKey = 'auto_op_key';

/// Counts up after each write or removal of the 75auto command key, so an
/// open 75AUTO page reads again whether a key is stored. It never carries
/// the key.
final autoOpKeyChanges = ValueNotifier<int>(0);

class CredentialStore {
  final _secure = const FlutterSecureStorage();

  // Android + iOS use the platform keystore/keychain; web falls back to
  // SharedPreferences (no secure storage available in the browser).
  bool get _useSecureStorage => !kIsWeb && (Platform.isAndroid || Platform.isIOS);

  Future<Map<String, String?>> readAll() async {
    if (_useSecureStorage) {
      return Map.of(await _secure.readAll())..remove(autoOpKeyStorageKey);
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
      'vhfcam_base_url': prefs.getString('vhfcam_base_url'),
    };
  }

  /// The 75auto command key as 32 bytes, or null when no valid key is
  /// stored. [readAll] does not return it, so the key text does not travel
  /// with the other settings.
  Future<Uint8List?> readAutoOpKey() async {
    final String? text;
    if (_useSecureStorage) {
      text = await _secure.read(key: autoOpKeyStorageKey);
    } else {
      final prefs = await SharedPreferences.getInstance();
      text = prefs.getString(autoOpKeyStorageKey);
    }
    return text == null ? null : parseAutoOpKey(text);
  }

  /// Stores the 75auto command key. [hex] must be 64 hex digits. Returns
  /// false, and stores nothing, for any other text.
  ///
  /// On Android and iOS the key goes to the keystore or the keychain. On the
  /// other platforms (web, desktop) it goes to SharedPreferences, like the
  /// broker password.
  Future<bool> writeAutoOpKey(String hex) async {
    if (parseAutoOpKey(hex) == null) return false;
    final text = hex.trim().toLowerCase();
    if (_useSecureStorage) {
      await _secure.write(key: autoOpKeyStorageKey, value: text);
    } else {
      final prefs = await SharedPreferences.getInstance();
      await prefs.setString(autoOpKeyStorageKey, text);
    }
    autoOpKeyChanges.value++;
    return true;
  }

  /// Removes the 75auto command key.
  Future<void> clearAutoOpKey() async {
    if (_useSecureStorage) {
      await _secure.delete(key: autoOpKeyStorageKey);
    } else {
      final prefs = await SharedPreferences.getInstance();
      await prefs.remove(autoOpKeyStorageKey);
    }
    autoOpKeyChanges.value++;
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
