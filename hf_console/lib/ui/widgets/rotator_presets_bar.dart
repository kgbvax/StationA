import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';

/// One-tap direction shortcuts for the rotator (NA / SA / VK / JA / STOP).
///
/// Used to live at the bottom of the compass card; moved to its own card so
/// the compass disc can use the full card height. The bar still rides the
/// same chrome as the rest of the HF page (card background, mono labels,
/// `AppTheme.actionButton`) and reads `rotator.isOnline` + `MqttService`
/// from the provider tree the same way the compass panel does.
///
/// Disabled when the rotator bridge is offline — same gating as the
/// tap-to-aim gesture on the disc, so the two surfaces stay in sync.
class RotatorPresetsBar extends StatelessWidget {
  const RotatorPresetsBar({super.key});

  @override
  Widget build(BuildContext context) {
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
          _Preset('NA 330', onPressed: rotatorOnline ? () => sendAz(330) : null),
          _Preset('SA 210', onPressed: rotatorOnline ? () => sendAz(210) : null),
          _Preset('VK 60', onPressed: rotatorOnline ? () => sendAz(60) : null),
          _Preset('JA 35', onPressed: rotatorOnline ? () => sendAz(35) : null),
          _Preset('STOP', danger: true, onPressed: rotatorOnline ? sendStop : null),
        ],
      ),
    );
  }
}

/// Tighter than the global `actionButton` (40 dp) — the rotator shortcuts
/// bar is a quick-access row that lives below the compass; we keep the same
/// compressed chrome the compass used so the layout reads consistently. The
/// hit area (44×32) sits above the iOS / Material 48 dp guideline's spirit
/// without dominating the row height.
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
