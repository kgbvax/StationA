import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/vhfcam/radio_status.dart';

void main() {
  test('parses the full /api/radio-status payload', () {
    final s = RadioStatus.fromJson(jsonDecode('''
      {
        "bridge_online": true,
        "session_held": false,
        "radio_ready": true,
        "audio_stream": false,
        "hint": "radio idle — tap RADIO AUDIO ON to start the RX stream",
        "leds": {"bridge": "on", "session": "off", "radio": "warn", "audio": "off"}
      }
    ''') as Map<String, dynamic>);

    expect(s.bridgeOnline, isTrue);
    expect(s.sessionHeld, isFalse);
    expect(s.radioReady, isTrue);
    expect(s.audioStream, isFalse);
    expect(s.hint, contains('RADIO AUDIO ON'));
    expect(s.leds['bridge'], 'on');
    expect(s.leds['session'], 'off');
    expect(s.leds['radio'], 'warn');
    expect(s.leds['audio'], 'off');
  });

  test('missing leds map falls back to unk, absent hint to empty', () {
    final s = RadioStatus.fromJson({'bridge_online': true});
    expect(s.leds, {'bridge': 'unk', 'session': 'unk', 'radio': 'unk', 'audio': 'unk'});
    expect(s.hint, isEmpty);
    expect(s.audioStream, isFalse);
  });

  test('non-bool fields do not crash the parse', () {
    final s = RadioStatus.fromJson({
      'bridge_online': 'yes', // server bug guard: treat as false
      'hint': 42,
      'leds': 'not-a-map',
    });
    expect(s.bridgeOnline, isFalse);
    expect(s.hint, isEmpty);
    expect(s.leds['bridge'], 'unk');
  });
}
