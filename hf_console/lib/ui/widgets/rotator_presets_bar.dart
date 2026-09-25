import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';

/// One-tap STOP shortcut for the rotator.
///
/// Tablet: [RotatorPresetsRail] — a vertical rail on the right edge of the DX
/// map, stacked above the +/- zoom controls, so the map column no longer
/// spends a footer row on presets and the disc gets the full card height.
/// Phone: [RotatorPresetsBar] — the horizontal bar stays in the scrolling
/// controls column; the phone map is too small to overlay a rail.
///
/// Both ride the same chrome as the rest of the HF page (card background,
/// mono labels, `AppTheme.actionButton`) and read `rotator.isOnline` +
/// `MqttService` from the provider tree the same way the compass panel does.
///
/// Disabled when the rotator bridge is offline — same gating as the
/// tap-to-aim gesture on the disc, so the two surfaces stay in sync.
class RotatorPresetsBar extends StatelessWidget {
  /// Which rotator STOP halts (default: the HF rotator).
  final RotatorSurface rotator;

  const RotatorPresetsBar({super.key, this.rotator = hfRotator});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.fromLTRB(12, 8, 12, 8),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border(top: BorderSide(color: AppTheme.cardLine)),
      ),
      child: Wrap(
        spacing: 6,
        runSpacing: 6,
        alignment: WrapAlignment.center,
        crossAxisAlignment: WrapCrossAlignment.center,
        children: [
          for (final a in _presetActions(context, rotator))
            _Preset(a.label, danger: a.danger, onPressed: a.onPressed),
        ],
      ),
    );
  }
}

/// Vertical direction-preset rail for the DX map's right edge (tablet
/// layout). Sits directly above the +/- zoom stepper (compass) / zoom row
/// (Mercator) and uses the same translucent card chrome as the rest of the
/// map overlay, so it reads as map chrome rather than a panel.
class RotatorPresetsRail extends StatelessWidget {
  /// Which rotator STOP halts (default: the HF rotator). The VHF surface
  /// stops both sat axes.
  final RotatorSurface rotator;

  const RotatorPresetsRail({super.key, this.rotator = hfRotator});

  @override
  Widget build(BuildContext context) {
    final actions = _presetActions(context, rotator);
    return Container(
      padding: const EdgeInsets.all(4),
      decoration: BoxDecoration(
        color: AppTheme.card.withValues(alpha: 0.85),
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(4),
      ),
      // IntrinsicWidth so the stretch Column gets a finite width inside the
      // unbounded Positioned — every button then shares the widest label.
      child: IntrinsicWidth(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            for (int i = 0; i < actions.length; i++) ...[
              _Preset(actions[i].label,
                  danger: actions[i].danger, onPressed: actions[i].onPressed),
              if (i < actions.length - 1) const SizedBox(height: 3),
            ],
          ],
        ),
      ),
    );
  }
}

class _PresetAction {
  final String label;
  final bool danger;
  final VoidCallback? onPressed;
  const _PresetAction(this.label, {this.danger = false, this.onPressed});
}

/// The stop action, gated on rotator-bridge liveness — shared by the
/// horizontal bar and the map-edge rail so the two stay in sync. Enabled
/// while any of the surface's stop slots is online (the sat panel rule: an
/// e-stop must outlive one dead axis); it stops every slot. Labelled STOP
/// everywhere — the same action as the sat panel's STOP key, so one name.
List<_PresetAction> _presetActions(BuildContext context, RotatorSurface rotator) {
  final store = context.watch<BusStore>();
  final mqtt = context.read<MqttService>();
  final anyOnline = store.linkUp &&
      rotator.stopSlots.any((slot) => store.slots['muehle/$slot']?.isOnline ?? false);

  void sendStop() {
    for (final slot in rotator.stopSlots) {
      mqtt.publish(cmdTopic(slot), rotator.stopPayload(), retain: cmdRetain['muehle/$slot'] ?? false);
    }
  }

  return [
    _PresetAction('STOP', danger: true, onPressed: anyOnline ? sendStop : null),
  ];
}

/// Tighter than the global `actionButton` (40 dp) — the rotator shortcuts
/// are quick-access controls; we keep the same compressed chrome so the
/// layout reads consistently. The hit area (44×32) sits above the iOS /
/// Material 48 dp guideline's spirit without dominating the row / rail
/// height.
class _Preset extends StatelessWidget {
  final String label;
  final bool danger;
  final VoidCallback? onPressed;

  const _Preset(this.label, {this.danger = false, this.onPressed});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: onPressed,
      style: AppTheme.actionButton(danger: danger).copyWith(
        padding: const WidgetStatePropertyAll(
          EdgeInsets.symmetric(horizontal: 12, vertical: 6),
        ),
        minimumSize: const WidgetStatePropertyAll(Size(44, 35)),
      ),
      child: Text(label),
    );
  }
}