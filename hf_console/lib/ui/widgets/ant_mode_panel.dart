import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/ant_mode.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'status_pill.dart';

/// Station antenna mode (`muehle/hf/ant-mode`, antmode): the normal / storm /
/// maintenance selector, what the lock is doing (phase, park, drift), the
/// inputs behind an automatic storm (presence, DWD wind and warnings), and
/// the storm suggestion with Enter storm / Dismiss.
///
/// The mode shown is the retained snapshot even while antmode's /status is
/// offline — a dead owner never unlocks, and the bridges keep enforcing it.
/// Commands need antmode online (they are one-shot requests it applies).
class AntModePanel extends StatelessWidget {
  const AntModePanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();
    final m = store.antMode;
    bool isOnline() => (store.slots[antModeSlot]?.isOnline ?? false) && store.linkUp;
    final online = isOnline();
    final failed = m.phase == 'failed' || m.fault.isNotEmpty;

    // Re-reads liveness at send time: a confirmation dialog may have been
    // open while antmode (or our link) went away. The payload's `ts` bounds
    // what slips through before the LWT arrives (see wiring.dart).
    void send(String payload) {
      if (!isOnline()) return;
      mqtt.publish(cmdTopic('hf/ant-mode'), payload, retain: cmdRetain[antModeSlot]!);
    }

    Future<void> setMode(String mode) async {
      if (!online || mode == m.mode) return;
      final ok = await _confirm(context, mode, m);
      if (ok == true) send(antModePayload(mode));
    }

    Future<void> repark() async {
      if (!online || !m.locked) return;
      final ok = await _confirmRepark(context, m);
      if (ok == true) send(antModeReparkPayload());
    }

    final (suffix, suffixColor) = failed
        ? ('PARK FAILED', AppTheme.red)
        : m.locked
        ? (m.label, AppTheme.amber)
        : m.stormSuggested
        ? ('STORM SUGGESTED', AppTheme.amber)
        : ('', null);

    // Compact status lines (mono, one fact per segment): the strip sits
    // under the antenna switch, so in normal it costs one line of compass.
    final lines = <(String, Color)>[
      // With antmode down (or our link) the selector is disabled and shows
      // no active mode, and the pill reads OFFLINE: name the mode here.
      if (!online)
        (m.locked ? '${m.label} · still enforced by the bridges' : m.label, m.locked ? AppTheme.amber : AppTheme.txtMute),
      if (m.locked)
        (
          [
            m.phase.isEmpty ? '—' : m.phase.toUpperCase(),
            _park(m),
            if (m.source.isNotEmpty) m.source,
            if (m.since.isNotEmpty) 'since ${_hhmm(m.since)}',
            if (m.autoExitAt.isNotEmpty) 'auto exit ${_hhmm(m.autoExitAt)}',
          ].join(' · '),
          failed ? AppTheme.red : AppTheme.txt,
        ),
      // The borrow is power_borrow, and may outlive the mode (an abort to
      // normal waits for powerseq to send the restore). power_hold alone
      // only says powerseq's idle logic is held (all of maintenance).
      if (m.powerBorrow != null || m.powerHold)
        (
          [
            if (m.powerBorrow case final b?) ...[
              'antenna power borrowed',
              if (b.takenOver) 'powerseq took over' else if (b.restoreTo.isNotEmpty) 'restores ${b.restoreTo}',
            ],
            if (m.powerHold) 'powerseq held',
          ].join(' · '),
          AppTheme.txt,
        ),
      if (m.fault.isNotEmpty) (m.fault, AppTheme.red),
      (
        [_wind(m.wind), if (m.presence.isNotEmpty) m.presence].join(' · '),
        (m.wind?.stale ?? true) ? AppTheme.txtMute : AppTheme.txt,
      ),
      if (m.warnings.isNotEmpty)
        (
          m.warnings.map((w) => 'DWD ${w.event}${w.expires.isEmpty ? '' : ' until ${_hhmm(w.expires)}'}').join(' · '),
          AppTheme.amber,
        ),
    ];

    // The card is the outer Container so it always fills the layout width;
    // the pill floats over its top padding band (the antenna-switch idiom).
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine)),
      ),
      child: Stack(
        children: [
          Padding(
            // Top padding clears the corner-overlaid status pill.
            padding: const EdgeInsets.fromLTRB(12, 26, 12, 10),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              mainAxisSize: MainAxisSize.min,
              children: [
                if (m.stormSuggested && !m.locked) ...[
                  _Suggestion(
                    reason: m.reason.isNotEmpty
                        ? m.reason
                        : m.warnings.isNotEmpty
                        ? 'DWD ${m.warnings.first.event} active'
                        : 'a DWD wind warning is active',
                    onEnter: online ? () => setMode('storm') : null,
                    onDismiss: online ? () => send(antModeDismissPayload()) : null,
                  ),
                  const SizedBox(height: 8),
                ],
                // REPARK is pinned to the right, apart from the mode keys
                // (the RETRACT idiom): a re-park turns a brakeless rotator.
                Row(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Expanded(
                      child: Wrap(
                        spacing: 8,
                        runSpacing: 8,
                        children: [
                          for (final mode in antModes)
                            SizedBox(
                              width: 112,
                              child: ElevatedButton(
                                key: ValueKey('ant-mode-$mode'),
                                // The current mode stays enabled (a no-op) so
                                // it keeps its highlight — a disabled button
                                // drops it.
                                onPressed: online ? () => setMode(mode) : null,
                                // Normal is the regular state (cyan); a lock
                                // is a deliberate hold — amber, the
                                // manual-hold idiom.
                                style: AppTheme.actionButton(
                                  active: mode == m.mode && mode == 'normal',
                                  amberActive: mode == m.mode && mode != 'normal',
                                ),
                                child: Text(
                                  mode == 'maintenance' ? 'MAINT.' : mode.toUpperCase(),
                                  maxLines: 1,
                                  softWrap: false,
                                ),
                              ),
                            ),
                        ],
                      ),
                    ),
                    if (m.locked) ...[
                      const SizedBox(width: 8),
                      ElevatedButton(
                        key: const ValueKey('ant-mode-repark'),
                        onPressed: online ? repark : null,
                        style: AppTheme.actionButton(),
                        child: const Text('REPARK'),
                      ),
                    ],
                  ],
                ),
                const SizedBox(height: 6),
                for (final (text, color) in lines)
                  Text(
                    text,
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                    style: AppTheme.mono(12, color: color, weight: FontWeight.w600),
                  ),
              ],
            ),
          ),
          // The pill paints last so it sits on top of the card.
          Positioned(
            top: 6,
            right: 10,
            child: StatusPill(
              slots: const [antModeSlot],
              label: 'Ant mode',
              useMetaName: false,
              suffix: suffix.isEmpty ? null : suffix,
              suffixColor: suffixColor,
              // A failed park survives a dead owner.
              stickySuffix: true,
            ),
          ),
        ],
      ),
    );
  }

  static String _park(AntModeState m) {
    final park = m.l.parkAz;
    if (park == null) return 'park pending';
    final drift = m.driftDeg;
    return 'park $park°${drift == null ? '' : ' · drift ${drift > 0 ? '+' : ''}$drift°'}';
  }

  static String _wind(AntWind? w) {
    if (w == null || w.dirDeg == null) return 'wind: no data';
    final gust = w.gustKmh == null ? '' : ' · gust ${w.gustKmh} km/h';
    return 'wind from ${w.dirDeg}°$gust${w.stale ? ' · STALE' : ''}';
  }

  static Future<bool?> _confirm(BuildContext context, String mode, AntModeState m) {
    final (title, body) = switch (mode) {
      'storm' => (
          'Enter storm mode?',
          'Grounds the antenna switch, retracts the Ultrabeam elements and parks the rotator broadside '
              'to the wind (once). All antenna motion is refused until you return to normal.',
        ),
      'maintenance' => (
          'Enter maintenance mode?',
          'Grounds the antenna switch, retracts the Ultrabeam elements and parks the rotator at the '
              'maintenance preset. Only the console\'s ±5° nudge keys move the rotator until you return to normal.',
        ),
      _ => (
          'Return to normal?',
          'Releases the antenna lock. The elements re-extend only when antenna select follows the band '
              'again.${m.warnings.isNotEmpty ? ' The active warnings are acknowledged and will not '
                  'enter storm automatically again.' : ''}',
        ),
    };
    return showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title, style: AppTheme.mono(14, weight: FontWeight.w700)),
        content: Text(body),
        actions: [
          TextButton(onPressed: () => Navigator.of(ctx).pop(false), child: const Text('Cancel')),
          ElevatedButton(
            key: const ValueKey('ant-mode-confirm'),
            style: AppTheme.actionButton(amber: mode != 'normal'),
            onPressed: () => Navigator.of(ctx).pop(true),
            child: Text(mode == 'normal' ? 'Normal' : mode == 'storm' ? 'Storm' : 'Maintenance'),
          ),
        ],
      ),
    );
  }

  static Future<bool?> _confirmRepark(BuildContext context, AntModeState m) {
    return showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text('Park the antenna again?', style: AppTheme.mono(14, weight: FontWeight.w700)),
        content: Text(m.mode == 'storm'
            ? 'Recomputes the heading from the current wind and turns the rotator there.'
            : 'Turns the rotator back to the maintenance preset.'),
        actions: [
          TextButton(onPressed: () => Navigator.of(ctx).pop(false), child: const Text('Cancel')),
          ElevatedButton(
            key: const ValueKey('ant-mode-repark-confirm'),
            style: AppTheme.actionButton(),
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('Repark'),
          ),
        ],
      ),
    );
  }
}

/// HH:MM local of an RFC 3339 time; the raw text when it does not parse.
String _hhmm(String iso) {
  final t = DateTime.tryParse(iso)?.toLocal();
  if (t == null) return iso;
  String two(int n) => n.toString().padLeft(2, '0');
  return '${two(t.hour)}:${two(t.minute)}';
}

/// The storm suggestion: a warning is active and the station is manned, so
/// antmode asks instead of acting. Square strip (part of the card); the
/// buttons carry the rounding.
class _Suggestion extends StatelessWidget {
  final String reason;
  final VoidCallback? onEnter;
  final VoidCallback? onDismiss;

  const _Suggestion({required this.reason, this.onEnter, this.onDismiss});

  @override
  Widget build(BuildContext context) {
    return Container(
      key: const ValueKey('ant-mode-suggestion'),
      padding: const EdgeInsets.all(8),
      decoration: BoxDecoration(
        color: AppTheme.blend(AppTheme.amber, 0.12),
        border: Border.all(color: AppTheme.amber),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Text('STORM MODE SUGGESTED', style: AppTheme.mono(13, color: AppTheme.amber, weight: FontWeight.w700)),
          const SizedBox(height: 2),
          Text(reason, style: AppTheme.mono(12, color: AppTheme.txt)),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              ElevatedButton(
                key: const ValueKey('ant-mode-enter-storm'),
                onPressed: onEnter,
                style: AppTheme.actionButton(amber: true),
                child: const Text('ENTER STORM'),
              ),
              ElevatedButton(
                key: const ValueKey('ant-mode-dismiss'),
                onPressed: onDismiss,
                style: AppTheme.actionButton(),
                child: const Text('DISMISS'),
              ),
            ],
          ),
        ],
      ),
    );
  }
}
