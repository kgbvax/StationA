import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'mic_profile_row.dart';
import 'status_pill.dart';

class DvkPanel extends StatelessWidget {
  static const String slot = 'hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  const DvkPanel({super.key});

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
              // with the readout pill gone — the DVK buttons don't show it.
              Consumer<BusStore>(builder: (context, store, _) {
                final tx = store.stateValueAs<String>('muehle/$slot', 'tx');
                return StatusPill(
                  slots: const ['muehle/hf/radio'],
                  label: 'FLEX-8400',
                  suffix: tx == 'tx' ? 'TX' : null,
                  suffixColor: AppTheme.red,
                );
              }),
            ],
          ),
          const SizedBox(height: 8),
          _buildBandRow(context),
          const SizedBox(height: 8),
          const MicProfileRow(),
          const SizedBox(height: 8),
          _buildButtonRow(context),
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
        final mqtt = context.read<MqttService>();

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
                    onPressed: online
                        ? () => mqtt.publish(topic, radioSetBandPayload('${band}m'), retain: false)
                        : null,
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
          const SizedBox(height: 3),
          Container(
            key: ValueKey('band-stripe-$band'),
            width: 18,
            height: 3,
            decoration: BoxDecoration(
              color: onPressed == null ? AppTheme.blend(stripe, 0.35) : stripe,
              borderRadius: BorderRadius.circular(1.5),
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildButtonRow(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = (slotState?.isOnline ?? false) && store.linkUp;
        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final activeId = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;
        final isPlaying = status == 'playback';

        final mqtt = context.read<MqttService>();

        return Row(
          children: [
            for (var i = 1; i <= 4; i++)
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.only(right: 8),
                  child: ElevatedButton(
                    onPressed: online ? () => mqtt.publish(topic, dvkPlayPayload(i), retain: false) : null,
                    style: AppTheme.actionButton(active: online && isPlaying && activeId == i).copyWith(
                      minimumSize: const WidgetStatePropertyAll(Size(0, 48)),
                      padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 6)),
                    ),
                    child: Text('DVK$i'),
                  ),
                ),
              ),
            ElevatedButton(
              onPressed: online ? () => mqtt.publish(topic, dvkStopPayload(), retain: false) : null,
              style: AppTheme.actionButton(danger: true).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(64, 48)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 6)),
              ),
              child: const Text('STOP'),
            ),
          ],
        );
      },
    );
  }
}
