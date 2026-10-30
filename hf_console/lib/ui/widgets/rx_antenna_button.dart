import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';

/// Receive antenna of the slice being listened on (flexbridge `/state.rx_input`:
/// the active slice, slice A in split), as one toggle: ANT 1 ↔ RX A.
///
/// The label is always the radio's own value, so a change made in SmartSDR
/// shows up here; a tap sends `select_rx` and the label moves only when the
/// radio confirms on `/state` (no optimistic flip, like the SSB/CW toggle).
/// RX A is lit; ANT 1 is the plain resting state; anything else (ANT 2, XVTA)
/// is amber because the console never sets it. Receive path only: the TX
/// antenna and the antenna-select / PA chain are untouched.
///
/// Disabled (`—`) while the radio or link is down, or when the bridge does not
/// publish `rx_input` (a flexbridge older than select_rx).
class RxAntennaButton extends StatelessWidget {
  static const String address = 'muehle/hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  const RxAntennaButton({super.key});

  /// Display name of a canonical rx_input value: `rx_a` → `RX A`, `ant1` →
  /// `ANT 1`; unknown values are upper-cased as-is.
  static String label(String input) => switch (input) {
        'ant1' => 'ANT 1',
        'ant2' => 'ANT 2',
        'rx_a' => 'RX A',
        'xvta' => 'XVTA',
        _ => input.toUpperCase().replaceAll('_', ' '),
      };

  @override
  Widget build(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final online = (store.slots[address]?.isOnline ?? false) && store.linkUp;
        final input = store.stateValueAs<String>(address, 'rx_input');
        final known = input != null && input.isNotEmpty;
        final enabled = online && known;
        final isRxA = known && input == 'rx_a';
        final unusual = known && input != 'rx_a' && input != 'ant1';

        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            // Caption sits on the same line as the MIC caption next to it.
            Text('RX ANT',
                style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
            const SizedBox(height: 4),
            Semantics(
              label: known ? 'Receive antenna ${label(input)}' : 'Receive antenna unknown',
              hint: enabled ? 'Switch to ${label(toggledRxInput(input))}' : null,
              child: ElevatedButton(
                key: const ValueKey('rx-antenna-btn'),
                onPressed: enabled
                    ? () => context
                        .read<MqttService>()
                        .publish(topic, radioSelectRxPayload(toggledRxInput(input)), retain: false)
                    : null,
                style: AppTheme.actionButton(active: isRxA, amber: unusual).copyWith(
                  minimumSize: const WidgetStatePropertyAll(Size(0, 48)),
                  padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 6, vertical: 6)),
                ),
                child: FittedBox(
                  fit: BoxFit.scaleDown,
                  child: Text(known ? label(input) : '—', maxLines: 1),
                ),
              ),
            ),
          ],
        );
      },
    );
  }
}
