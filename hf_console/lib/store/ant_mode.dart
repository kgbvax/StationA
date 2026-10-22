/// The station antenna mode — `muehle/hf/ant-mode` (component antmode), the
/// console side of `../antmode/docs/ant-mode-mqtt-api.md`.
///
/// In storm and maintenance the Ultrabeam and the HF rotator are parked and
/// locked: the bridges refuse motion themselves (wrc-rotator-bridge,
/// ultrabridge) and antennaselect grounds the switch. The console only
/// mirrors the lock — it greys out what the bridges would refuse and says
/// why, so a tap is never silently dropped.
///
/// Reading rules (the same as `shared/antmode.Parse`, which every Go
/// consumer uses):
///
/// - Empty / no retained state → normal (the station is usable before
///   antmode exists).
/// - A valid snapshot with `ant_mode` ≠ normal is enforced even while
///   `hf/ant-mode/status` is offline — a dead owner never unlocks.
/// - A malformed snapshot keeps the last known state ([BusStore] does that).
/// - `ts` is not checked: this is steady state.
library;

const antModeSlot = 'muehle/hf/ant-mode';

/// The valid modes, in display order (the /meta enum options).
const antModes = ['normal', 'storm', 'maintenance'];

/// The `override` tag on a rotator set_az that asks for a maintenance nudge
/// (`shared/antmode.NudgeOverride`). Intent tagging, not authentication.
const nudgeOverride = 'maintenance';

/// What the enforcing bridges do while the mode is not normal.
class AntLock {
  /// Identifies one park request; a new value means "park again".
  final int seq;

  /// antennaselect holds the antenna switch at off.
  final bool ground;

  /// ultrabridge retracts the elements and keeps them in.
  final bool retract;

  /// wrc-rotator-bridge drives here once (null = not yet).
  final int? parkAz;

  /// A console set_az tagged [nudgeOverride] is allowed (maintenance only).
  final bool nudge;

  const AntLock({this.seq = 0, this.ground = false, this.retract = false, this.parkAz, this.nudge = false});
}

/// DWD MOSMIX day wind for the configured station.
class AntWind {
  final int? dirDeg;
  final int? gustKmh;
  final String ts;
  final bool stale;

  const AntWind({this.dirDeg, this.gustKmh, this.ts = '', this.stale = true});
}

/// One active qualifying DWD warning.
class AntWarning {
  final String id;
  final String event;
  final String ii;
  final String onset;
  final String expires;

  const AntWarning({required this.id, this.event = '', this.ii = '', this.onset = '', this.expires = ''});
}

class AntModeState {
  /// `normal` | `storm` | `maintenance`.
  final String mode;

  /// `manual` | `auto` (who entered the current mode); '' when unknown.
  final String source;
  final String since;

  /// Present whenever [mode] ≠ normal (a locked mode without a lock block
  /// reads as a zero lock: refuse motion, nothing parked yet).
  final AntLock? lock;
  final String phase;
  final String fault;
  final bool powerHold;
  final String presence;
  final AntWind? wind;
  final List<AntWarning> warnings;
  final bool stormSuggested;
  final String reason;
  final String autoExitAt;
  final int? driftDeg;

  const AntModeState({
    this.mode = 'normal',
    this.source = '',
    this.since = '',
    this.lock,
    this.phase = '',
    this.fault = '',
    this.powerHold = false,
    this.presence = '',
    this.wind,
    this.warnings = const [],
    this.stormSuggested = false,
    this.reason = '',
    this.autoExitAt = '',
    this.driftDeg,
  });

  static const normal = AntModeState();

  /// Motion must be refused (storm or maintenance).
  bool get locked => mode != 'normal';

  /// The lock, never null (zero lock when unlocked or not yet built).
  AntLock get l => lock ?? const AntLock();

  /// A console nudge (set_az with [nudgeOverride]) is allowed.
  bool get nudgeAllowed => mode == 'maintenance' && l.nudge;

  /// The upper-case mode word for badges: STORM / MAINTENANCE.
  String get label => mode.toUpperCase();

  /// The one-line operator reason for a refused control, '' when unlocked.
  /// Kept short: it rides in card pill bands and the compass chip.
  ///
  /// Only antmode's `phase` may claim the park: `lock.park_az` is the
  /// target, published as the rotation starts, and it stays on a failed
  /// park. So "parked" needs phase `parked`, a failure says so, and
  /// anything else is still on its way.
  String get lockReason {
    if (!locked) return '';
    final park = l.parkAz;
    final where = switch (phase) {
      'parked' => park == null ? 'parked' : 'parked $park°',
      'failed' => 'PARK FAILED',
      _ => park == null ? 'parking' : 'parking → $park°',
    };
    return '$label LOCK · $where';
  }

  /// Parses a /state snapshot. Returns [normal] for null (no retained
  /// state), and null for a malformed snapshot (the caller keeps its last
  /// known state).
  static AntModeState? parse(Map<String, dynamic>? s) {
    if (s == null) return normal;
    final mode = s['ant_mode'];
    if (mode is! String || !antModes.contains(mode)) return null;
    AntLock? lock;
    if (mode != 'normal') {
      final raw = s['lock'];
      if (raw != null && raw is! Map) return null;
      if (raw is Map) {
        final park = raw['park_az'];
        if (park != null && (park is! num || park < 0 || park > 450)) return null;
        lock = AntLock(
          seq: _int(raw['seq']) ?? 0,
          ground: raw['ground'] == true,
          retract: raw['retract'] == true,
          parkAz: park is num ? park.round() : null,
          nudge: raw['nudge'] == true,
        );
      } else {
        lock = const AntLock();
      }
    }
    final w = s['wind'];
    final warnings = <AntWarning>[];
    final ws = s['warnings'];
    if (ws is List) {
      for (final e in ws) {
        if (e is! Map) continue;
        warnings.add(AntWarning(
          id: _str(e['id']),
          event: _str(e['event']),
          ii: _str(e['ii']),
          onset: _str(e['onset']),
          expires: _str(e['expires']),
        ));
      }
    }
    return AntModeState(
      mode: mode,
      source: _str(s['source']),
      since: _str(s['since']),
      lock: lock,
      phase: _str(s['phase']),
      fault: _str(s['fault']),
      powerHold: s['power_hold'] == true,
      presence: _str(s['presence']),
      wind: w is Map
          ? AntWind(
              dirDeg: _int(w['dir_deg']),
              gustKmh: _int(w['gust_kmh']),
              ts: _str(w['ts']),
              stale: w['stale'] != false,
            )
          : null,
      warnings: warnings,
      stormSuggested: s['storm_suggested'] == true,
      reason: _str(s['reason']),
      autoExitAt: _str(s['auto_exit_at']),
      driftDeg: _int(s['drift_deg']),
    );
  }

  static String _str(dynamic v) => v is String ? v : '';
  static int? _int(dynamic v) => v is num ? v.round() : null;
}
