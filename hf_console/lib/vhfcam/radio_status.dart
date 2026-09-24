// radio_status.dart — the /api/radio-status payload of vhfcam-restream.
//
// vhfcam-restream's preview server on :8083 answers GET /api/radio-status with
// the live IC-9700 audio-chain state it derives from the bus (radio-bridge
// session + LWT) and its own UDP audio monitor. The console polls it every 2 s
// from the CAM page — it is the same JSON the :8083 reference page renders as
// its status LEDs, so the console never guesses radio state from MQTT planes
// the vhfcam slot does not publish.
//
// leds mirrors the server's per-LED strings exactly: bridge on|off, session
// on|off|unk, radio on|warn|unk, audio on|off. Unknown keys are preserved so a
// future fourth LED still renders.

class RadioStatus {
  /// icom9700-radio-bridge reachable (its /status LWT is fresh).
  final bool bridgeOnline;

  /// The bridge currently holds the CI-V session (audio demand active).
  final bool sessionHeld;

  /// The radio answers CI-V.
  final bool radioReady;

  /// Real radio PCM arrived on the preview's UDP socket < 3 s ago.
  final bool audioStream;

  /// Server-supplied operator hint (why the chain is where it is).
  final String hint;

  /// LED name → server state string (`on`/`off`/`warn`/`unk`).
  final Map<String, String> leds;

  const RadioStatus({
    required this.bridgeOnline,
    required this.sessionHeld,
    required this.radioReady,
    required this.audioStream,
    required this.hint,
    required this.leds,
  });

  factory RadioStatus.fromJson(Map<String, dynamic> json) {
    String led(String name) {
      final v = json['leds'];
      if (v is Map<String, dynamic>) {
        final s = v[name];
        if (s is String) return s;
      }
      return 'unk';
    }

    return RadioStatus(
      bridgeOnline: json['bridge_online'] == true,
      sessionHeld: json['session_held'] == true,
      radioReady: json['radio_ready'] == true,
      audioStream: json['audio_stream'] == true,
      hint: json['hint'] is String ? json['hint'] as String : '',
      leds: {
        'bridge': led('bridge'),
        'session': led('session'),
        'radio': led('radio'),
        'audio': led('audio'),
      },
    );
  }
}
