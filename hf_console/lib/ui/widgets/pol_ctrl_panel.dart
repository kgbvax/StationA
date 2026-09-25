import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'pol_glyph.dart';
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
  /// Short button labels; the long names are for the readout.
  static const _short = {'h': 'H', 'v': 'V', 'cl': 'LHCP', 'cr': 'RHCP'};
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
      final active = pol == p;
      // The glyph takes the button's foreground colour (dark on the active
      // accent fill, faint when disabled) so it reads like the label.
      final fg = !online
          ? AppTheme.txtFaint
          : active
              ? AppTheme.activeButtonText
              : AppTheme.txt;
      buttons.add(Expanded(
        child: ElevatedButton(
          key: ValueKey('pol-btn-$p'),
          onPressed: online ? () => setPol(p) : null,
          style: AppTheme.actionButton(active: active).copyWith(
            padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 4, vertical: 8)),
          ),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              PolGlyph(pol: p, color: fg, size: 26),
              const SizedBox(height: 4),
              FittedBox(
                fit: BoxFit.scaleDown,
                child: Text(_short[p]!, maxLines: 1, style: AppTheme.mono(12, weight: FontWeight.w800)),
              ),
            ],
          ),
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
              // Fallback when /meta has no expose name: the controller, not
              // the card title again.
              label: 'StamPLC #2',
              suffix: errorText != null ? 'ERR' : null,
              suffixColor: AppTheme.red,
              stickySuffix: true, // ERR survives a dead link
            ),
          ),
          const SizedBox(height: 12),
          Row(
            children: [
              if (PolGlyph.supports(pol)) ...[
                PolGlyph(key: const ValueKey('pol-readout-glyph'), pol: pol!, color: AppTheme.accent, size: 34),
                const SizedBox(width: 10),
              ],
              Text(
                phaseLabel,
                style: AppTheme.mono(20, weight: FontWeight.w700),
              ),
              const Spacer(),
              // Flexible: on a narrow card the note ellipsizes instead of
              // running past the card edge.
              Flexible(
                child: Text(
                  'both X-Quads',
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: AppTheme.mono(10, color: AppTheme.txtFaint, weight: FontWeight.w600),
                ),
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