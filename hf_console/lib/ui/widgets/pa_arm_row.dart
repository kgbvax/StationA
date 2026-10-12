import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'status_tag.dart';

const _slot = 'muehle/hf/pa-arm';

/// The PA arm interlock (PLC #1 relay 1) as a row inside the PA card.
///
/// Read-only annunciator first: the arm is firmware-owned (gates in the PLC's
/// arm.h), so a disabled or blocked arm must be visible even though it is
/// not a fault — FAULTS stays ALL OK while the PA cannot transmit. `block`
/// (normal transitions: band change settling, Ultrabeam moving, permit off,
/// waiting for RX) is shown here only and never enters the fault history;
/// `error` (the fault subset) reaches FAULTS through the store as usual.
///
/// The ARM PERMIT button only sets the permit (`set_enabled`, retained) —
/// it never forces the relay; every firmware gate still applies. It asks for
/// confirmation because the permit is a sequencer-owned safety input
/// (powerseq STOP retracts it) and a stray tap must not change it.
class PaArmRow extends StatelessWidget {
  const PaArmRow({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final online = (store.slots[_slot]?.isOnline ?? false) && store.linkUp;
    final enabled = store.stateValueAs<bool>(_slot, 'enabled');
    final armed = store.stateValueAs<bool>(_slot, 'armed');
    final block = store.stateValueAs<String>(_slot, 'block')?.trim() ?? '';

    final (label, color) = paArmAnnunciator(
      online: online,
      enabled: enabled,
      armed: armed,
      block: block,
    );

    // Unknown permit: no definite toggle target, so no command (R4.4).
    final canToggle = online && enabled != null;

    Future<void> togglePermit() async {
      final grant = !enabled!;
      final ok = await showDialog<bool>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: Text(grant ? 'Grant arm permit?' : 'Revoke arm permit?',
              style: AppTheme.mono(14, weight: FontWeight.w700)),
          content: Text(grant
              ? 'The PLC arms the PA once every interlock gate is clear '
                  'and the radio is in RX. The permit is retained on the bus.'
              : 'The PLC opens the PA arm relay. The PA cannot transmit '
                  'until the permit is granted again (console, PLC button A '
                  'or START STATION).'),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(ctx).pop(false),
              child: const Text('Cancel'),
            ),
            ElevatedButton(
              style: grant ? AppTheme.actionButton(active: true) : AppTheme.actionButton(danger: true),
              onPressed: () => Navigator.of(ctx).pop(true),
              child: Text(grant ? 'GRANT' : 'REVOKE'),
            ),
          ],
        ),
      );
      if (ok != true) return;
      // Re-check after the dialog: the link or slot may have dropped while
      // it was open.
      if (!context.mounted) return;
      final now = context.read<BusStore>();
      if (!((now.slots[_slot]?.isOnline ?? false) && now.linkUp)) return;
      mqtt.publish(
        cmdTopic('hf/pa-arm'),
        paArmPayload(grant),
        retain: cmdRetain[_slot]!,
      );
    }

    return Row(
      children: [
        Text('ARM', style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
        const SizedBox(width: 8),
        Flexible(
          child: Align(
            alignment: Alignment.centerLeft,
            child: StatusTag(key: const ValueKey('pa-arm-tag'), label: label, color: color),
          ),
        ),
        const SizedBox(width: 10),
        SizedBox(
          width: 96,
          height: 37,
          child: ElevatedButton(
            onPressed: canToggle ? togglePermit : null,
            style: AppTheme.actionButton(active: enabled == true),
            child: const FittedBox(child: Text('ARM PERMIT')),
          ),
        ),
      ],
    );
  }
}

/// Annunciator label and color for the pa-arm slot.
///
/// ARMED (green) is the relay readback; PERMIT OFF (grey) is the operator
/// fact to act on even when a gate would also block; `BLOCKED: <block>`
/// (amber) is the first failing gate while permitted. Missing keys stay
/// unknown — never invented as SAFE or ARMED (R4.4).
(String, Color) paArmAnnunciator({
  required bool online,
  required bool? enabled,
  required bool? armed,
  required String block,
}) {
  if (!online) return ('OFFLINE', AppTheme.txtMute);
  if (armed == true) {
    // Closed but a readiness gate is pending (held only through TX) — the
    // relay will open at the next RX, so it is not a plain green.
    if (block.isNotEmpty) return ('ARMED · ${block.toUpperCase()}', AppTheme.amber);
    return ('ARMED', AppTheme.green);
  }
  if (enabled == false) return ('PERMIT OFF', AppTheme.txtMute);
  if (enabled == null || armed == null) return ('ARM ?', AppTheme.amber);
  if (block.isNotEmpty) return ('BLOCKED: ${block.toUpperCase()}', AppTheme.amber);
  return ('NOT ARMED', AppTheme.amber);
}
