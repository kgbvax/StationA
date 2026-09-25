// rec_status.dart — the recorder block of vhfcam-restream's status payload
// (`rec` in GET /api/radio-status). The server records what its preview
// shows (overlay + IC-9700 audio); the console only starts/stops and shows
// progress. Absent block (older server) = recording not available.

class RecStatus {
  /// Recording is possible at all (server has [record] and the preview on).
  final bool enabled;

  /// `idle` | `recording` | `finalizing`.
  final String state;

  /// File the active recording becomes (empty when idle).
  final String name;
  final int elapsedS;
  final int remainingS;
  final int maxS;
  final int freeBytes;
  final int minFreeBytes;

  /// No new video from the preview for a while (recording continues).
  final bool stalled;

  /// Why the last recording ended: operator | max_duration | low_disk |
  /// shutdown | recovered.
  final String stopReason;
  final String lastError;
  final String lastFile;

  const RecStatus({
    required this.enabled,
    required this.state,
    this.name = '',
    this.elapsedS = 0,
    this.remainingS = 0,
    this.maxS = 0,
    this.freeBytes = 0,
    this.minFreeBytes = 0,
    this.stalled = false,
    this.stopReason = '',
    this.lastError = '',
    this.lastFile = '',
  });

  bool get recording => state == 'recording';
  bool get finalizing => state == 'finalizing';

  /// Null for a missing or malformed block — never a guessed idle state.
  static RecStatus? tryParse(Object? json) {
    if (json is! Map<String, dynamic>) return null;
    final state = json['state'];
    if (state is! String || state.isEmpty) return null;
    int n(String k) => json[k] is num ? (json[k] as num).toInt() : 0;
    String s(String k) => json[k] is String ? json[k] as String : '';
    return RecStatus(
      enabled: json['enabled'] == true,
      state: state,
      name: s('name'),
      elapsedS: n('elapsed_s'),
      remainingS: n('remaining_s'),
      maxS: n('max_s'),
      freeBytes: n('free_bytes'),
      minFreeBytes: n('min_free_bytes'),
      stalled: json['stalled'] == true,
      stopReason: s('stop_reason'),
      lastError: s('last_error'),
      lastFile: s('last_file'),
    );
  }
}
