import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'mic_profile_row.dart';

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
          Text('TRX · FLEX-8400'.toUpperCase(),
              style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
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
        return Wrap(
          spacing: 8,
          runSpacing: 8,
          children: bands.map((band) {
            final full = '${band}m';
            final active = currentBand == full;
            return ElevatedButton(
              onPressed: online ? () => mqtt.publish(topic, radioSetBandPayload(full), retain: false) : null,
              style: AppTheme.actionButton(active: active).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(84, 48)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 8)),
              ),
              child: Text(full),
            );
          }).toList(),
        );
      },
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
