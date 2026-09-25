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

  test('parses the rec block and audio holders', () {
    final s = RadioStatus.fromJson(jsonDecode('''
      {"leds": {}, "audio_holders": ["recording"],
       "rec": {"enabled": true, "state": "recording", "name": "vhfcam_2026-09-25T1812Z_435.000MHz.mp4",
               "elapsed_s": 252, "remaining_s": 1548, "max_s": 1800, "free_bytes": 41200000000,
               "min_free_bytes": 8000000000, "stalled": false}}
    ''') as Map<String, dynamic>);
    expect(s.audioHolders, ['recording']);
    expect(s.rec, isNotNull);
    expect(s.rec!.recording, isTrue);
    expect(s.rec!.elapsedS, 252);
    expect(s.rec!.remainingS, 1548);
    expect(s.rec!.freeBytes, 41200000000);
  });

  test('missing or malformed rec block is null, never a guessed idle', () {
    RadioStatus parse(String json) => RadioStatus.fromJson(jsonDecode(json) as Map<String, dynamic>);
    expect(parse('{"leds": {}}').rec, isNull);
    expect(parse('{"leds": {}, "rec": "yes"}').rec, isNull);
    expect(parse('{"leds": {}, "rec": {"elapsed_s": 3}}').rec, isNull);
    expect(parse('{"leds": {}}').audioHolders, isEmpty);
  });
}
