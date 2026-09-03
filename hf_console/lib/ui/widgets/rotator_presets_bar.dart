import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';

/// One-tap direction shortcuts for the rotator (NA / SA / VK / JA / STOP).
///
/// Tablet: [RotatorPresetsRail] — a vertical rail on the right edge of the DX
/// map, stacked above the +/- zoom controls, so the map column no longer
/// spends a footer row on presets and the disc gets the full card height.
/// Phone: [RotatorPresetsBar] — the horizontal bar stays in the scrolling
/// controls column; the phone map is too small to overlay a five-button rail.
///
/// Both ride the same chrome as the rest of the HF page (card background,
/// mono labels, `AppTheme.actionButton`) and read `rotator.isOnline` +
/// `MqttService` from the provider tree the same way the compass panel does.
///
/// Disabled when the rotator bridge is offline — same gating as the
/// tap-to-aim gesture on the disc, so the two surfaces stay in sync.
class RotatorPresetsBar extends StatelessWidget {
  const RotatorPresetsBar({super.key});

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
          for (final a in _presetActions(context))
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
  const RotatorPresetsRail({super.key});

  @override
  Widget build(BuildContext context) {
    final actions = _presetActions(context);
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

/// The five presets, gated on rotator-bridge liveness — shared by the
/// horizontal bar and the map-edge rail so the two stay in sync.
List<_PresetAction> _presetActions(BuildContext context) {
  final store = context.watch<BusStore>();
  final mqtt = context.read<MqttService>();
  final rotatorOnline = store.slots['muehle/hf/rotator']?.isOnline ?? false;

  void sendAz(double value) {
    mqtt.publish(
      cmdTopic('hf/rotator'),
      rotatorAzPayload(value),
      retain: cmdRetain['muehle/hf/rotator']!,
    );
  }

  void sendStop() {
    mqtt.publish(cmdTopic('hf/rotator'), rotatorStopPayload(), retain: false);
  }

  return [
    _PresetAction('NA 330', onPressed: rotatorOnline ? () => sendAz(330) : null),
    _PresetAction('SA 210', onPressed: rotatorOnline ? () => sendAz(210) : null),
    _PresetAction('VK 60', onPressed: rotatorOnline ? () => sendAz(60) : null),
    _PresetAction('JA 35', onPressed: rotatorOnline ? () => sendAz(35) : null),
    _PresetAction('STOP', danger: true, onPressed: rotatorOnline ? sendStop : null),
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
        minimumSize: const WidgetStatePropertyAll(Size(44, 32)),
      ),
      child: Text(label),
    );
  }
}