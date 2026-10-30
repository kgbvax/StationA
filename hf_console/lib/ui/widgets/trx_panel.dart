import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'mic_profile_row.dart';
import 'rx_antenna_button.dart';
import 'status_pill.dart';

/// The TRX card: the radio's settings — band row, receive antenna and mic
/// profiles. The DVK keys are their own panel ([DvkPanel]) at the bottom left
/// of the HF page, where a hand rests during a CQ run; this card holds what is
/// set once per band or QSO.
class TrxPanel extends StatelessWidget {
  static const String slot = 'hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  const TrxPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              Text('TRX',
                  style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
              // A live transmit is the one TRX state that must surface even
              // with the readout pill gone — the DVK keys don't show it.
              // Split (TX slice ≠ slice A) rides the same suffix in amber
              // while not keyed; the cross-band case has its own banner.
              Consumer<BusStore>(builder: (context, store, _) {
                final tx = store.stateValueAs<String>('muehle/$slot', 'tx') == 'tx';
                final split = store.stateValueAs<bool>('muehle/$slot', 'split') == true;
                return StatusPill(
                  slots: const ['muehle/hf/radio'],
                  label: 'FLEX-8400',
                  suffix: tx ? 'TX' : (split ? 'SPLIT' : null),
                  suffixColor: tx ? AppTheme.red : AppTheme.amber,
                );
              }),
            ],
          ),
          const SizedBox(height: 8),
          _buildBandRow(context),
          const SizedBox(height: 8),
          // RX antenna and mic profiles share one row: both are captioned
          // 48 dp buttons, so the captions and the buttons line up.
          const Row(
            crossAxisAlignment: CrossAxisAlignment.end,
            children: [
              SizedBox(width: 96, child: RxAntennaButton()),
              SizedBox(width: 8),
              Expanded(child: MicProfileRow()),
            ],
          ),
        ],
      ),
    );
  }

  Widget _buildBandRow(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = (slotState?.isOnline ?? false) && store.linkUp;
        final currentBand = store.stateValueAs<String>('muehle/$slot', 'band') ?? '';
        final mode = store.stateValueAs<String>('muehle/$slot', 'mode');
        final freqHz = store.stateValueAs<num>('muehle/$slot', 'freq_hz');
        final mqtt = context.read<MqttService>();

        // Tapping the band the radio is already on toggles CW <-> SSB
        // (sideband by frequency, toggledSsbCwMode) instead of re-stacking.
        void onBand(String band) {
          final payload = band == currentBand
              ? radioSetModePayload(toggledSsbCwMode(mode, freqHz))
              : radioSetBandPayload(band);
          mqtt.publish(topic, payload, retain: false);
        }

        const bands = ['80', '40', '20', '17', '15', '12', '10'];
        // One row of equal-width buttons (a Wrap split 5 + 2 on the tablet).
        // Each carries a short underline in its horstreporter band colour
        // (AppTheme.bandColor, fixed across themes) so a band button and the
        // spot dots of that band on the map read as the same thing.
        return Row(
          children: [
            for (final (i, band) in bands.indexed)
              Expanded(
                child: Padding(
                  padding: EdgeInsets.only(right: i < bands.length - 1 ? 6 : 0),
                  child: _bandButton(
                    '${band}m',
                    active: currentBand == '${band}m',
                    onPressed: online ? () => onBand('${band}m') : null,
                  ),
                ),
              ),
          ],
        );
      },
    );
  }

  Widget _bandButton(String band, {required bool active, VoidCallback? onPressed}) {
    final stripe = AppTheme.bandColor(band);
    return ElevatedButton(
      onPressed: onPressed,
      style: AppTheme.actionButton(active: active).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(0, 48)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 4, vertical: 6)),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          FittedBox(fit: BoxFit.scaleDown, child: Text(band, maxLines: 1)),
          const SizedBox(height: 4),
          // Full button width, 6 px tall: a short 3 px dash was too small to
          // tell the band colours apart at arm's length on the tablet.
          Container(
            key: ValueKey('band-stripe-$band'),
            width: double.infinity,
            height: 6,
            decoration: BoxDecoration(
              color: onPressed == null ? AppTheme.blend(stripe, 0.35) : stripe,
              borderRadius: BorderRadius.circular(3),
            ),
          ),
        ],
      ),
    );
  }
}
