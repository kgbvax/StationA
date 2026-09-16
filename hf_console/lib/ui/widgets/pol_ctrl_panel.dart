import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'status_pill.dart';

/// X-Quad polarization surface (U11): the Tier-2 four-state control for the
/// `muehle/uhf/pol-ctrl` slot published by m5stamp-pol-ctrl (M5 Stamp PLC #2).
/// One **shared** phase setting for both X-Quads — their phase lines are
/// driven by the same relay set, so there is exactly one selection, not one
/// per antenna.
///
/// Publishes the retained `{'action':'set_pol','value':'<pol>'}` payload on
/// tap (the station value-key convention). Retention is the deliberate
/// KTD13 contrast with the sat rotators' one-shot `goto`/`stop`: a phase is
/// desired **steady state**, so the retained cmd re-applies the operator's
/// last intent after a controller reboot or broker reconnect — the same
/// posture as the ant-switch actuator.
///
/// The selected state renders from the `/state` relay readback (`pol`),
/// never from local tap optimism (KTD15): until the device republishes, the
/// panel keeps showing the old phase. Gating is the station two-layer AND —
/// `slot.isOnline && store.linkUp` — so stale retained state can never
/// render operable.
///
/// R15: this surface is purely operator-driven. Nothing here binds
/// polarization to band, tracking, rotor position, or any policy — taps are
/// the only publish path. A rejected command surfaces in-panel as an ERR tag
/// (the firmware publishes rejections into the retained `/state` `error`
/// field) and, via the store, on the console faults bar.
class PolCtrlPanel extends StatelessWidget {
  const PolCtrlPanel({super.key});

  static const _slot = 'uhf/pol-ctrl';
  static const _address = 'muehle/uhf/pol-ctrl';

  /// Canonical vocabulary (m5stamp-pol-ctrl /meta capabilities.polarizations).
  static const _pols = ['h', 'v', 'cl', 'cr'];
  static const _labels = {
    'h': 'HORIZONTAL',
    'v': 'VERTICAL',
    'cl': 'CIRCULAR LEFT',
    'cr': 'CIRCULAR RIGHT',
  };

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots[_address];
    final online = (slot?.isOnline ?? false) && store.linkUp;

    // Relay readback is the truth. A missing `pol` renders a dash and a
    // value outside the vocabulary renders raw — never a coerced guess.
    final pol = store.stateValueAs<String>(_address, 'pol');
    final error = slot?.state?['error'];
    final errorText = error is String && error.isNotEmpty ? error : null;
    final phaseLabel = pol == null ? '—' : (_labels[pol] ?? pol);

    void setPol(String phase) {
      if (!online) return;
      mqtt.publish(
        cmdTopic(_slot),
        setPolPayload(phase),
        retain: cmdRetain[_address]!,
      );
    }

    final buttons = <Widget>[];
    for (var i = 0; i < _pols.length; i++) {
      if (i > 0) buttons.add(const SizedBox(width: 6));
      final p = _pols[i];
      buttons.add(Expanded(
        child: ElevatedButton(
          key: ValueKey('pol-btn-$p'),
          onPressed: online ? () => setPol(p) : null,
          style: AppTheme.actionButton(active: pol == p),
          child: Text(p.toUpperCase(),
              style: AppTheme.mono(13, weight: FontWeight.w800)),
        ),
      ));
    }

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'X-QUAD POLARIZATION',
            trailing: StatusPill(
              slots: const ['muehle/uhf/pol-ctrl'],
              label: 'X-Quad',
              suffix: errorText != null ? 'ERR' : null,
              suffixColor: AppTheme.red,
              stickySuffix: true, // ERR survives a dead link
            ),
          ),
          const SizedBox(height: 12),
          Row(
            children: [
              Text(
                phaseLabel,
                style: AppTheme.mono(20, weight: FontWeight.w700),
              ),
              const Spacer(),
              Text(
                'SHARED · BOTH X-QUADS',
                style: AppTheme.mono(10,
                    color: AppTheme.txtFaint, weight: FontWeight.w600),
              ),
            ],
          ),
          if (errorText != null) ...[
            const SizedBox(height: 4),
            Text(
              errorText,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.mono(11, color: AppTheme.red),
            ),
          ],
          const SizedBox(height: 10),
          Row(children: buttons),
        ],
      ),
    );
  }
}