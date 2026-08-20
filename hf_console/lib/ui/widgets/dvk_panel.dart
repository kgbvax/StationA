import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class DvkPanel extends StatelessWidget {
  static const String slot = 'hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  const DvkPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          _buildHeader(context),
          const SizedBox(height: 8),
          _buildButtonGrid(context),
        ],
      ),
    );
  }

  Widget _buildHeader(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = slotState?.isOnline ?? false;
        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final id = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;

        final (label, color, bg) = _statusStyle(status, online, id);

        return Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: [
            Text('DVK · FLEX-8400'.toUpperCase(),
                style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.12)),
            Container(
              padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
              decoration: BoxDecoration(
                color: bg,
                borderRadius: BorderRadius.circular(3),
                border: Border.all(color: color.withValues(alpha: 0.35)),
              ),
              child: Text(
                label,
                style: AppTheme.mono(9, color: color, weight: FontWeight.w600),
              ),
            ),
          ],
        );
      },
    );
  }

  (String, Color, Color) _statusStyle(String status, bool online, int id) {
    if (!online) {
      return ('OFFLINE', AppTheme.txtMute, const Color(0x08FFFFFF));
    }
    switch (status) {
      case 'playback':
        final label = id > 0 ? 'PLAYBACK · M$id' : 'PLAYBACK';
        return (label, AppTheme.cyan, const Color(0x1427D7D8));
      case 'recording':
        return ('RECORDING', AppTheme.amber, const Color(0x14E0A23C));
      case 'preview':
        return ('PREVIEW', AppTheme.amber, const Color(0x14E0A23C));
      case 'disabled':
        return ('DISABLED', AppTheme.red, const Color(0x14D9533A));
      case 'idle':
      default:
        return ('IDLE', AppTheme.txtMute, const Color(0x08FFFFFF));
    }
  }

  Widget _buildButtonGrid(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = slotState?.isOnline ?? false;
        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final activeId = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;
        final isPlaying = status == 'playback';

        final mqtt = context.read<MqttService>();

        return Wrap(
          spacing: 4,
          runSpacing: 4,
          children: [
            for (var i = 1; i <= 12; i++)
              _MemoryButton(
                id: i,
                active: online && isPlaying && activeId == i,
                enabled: online,
                onTap: () => mqtt.publish(topic, dvkPlayPayload(i), retain: false),
              ),
            _StopButton(
              enabled: online,
              onTap: () => mqtt.publish(topic, dvkStopPayload(), retain: false),
            ),
          ],
        );
      },
    );
  }
}

class _MemoryButton extends StatelessWidget {
  final int id;
  final bool active;
  final bool enabled;
  final VoidCallback onTap;

  const _MemoryButton({
    required this.id,
    required this.active,
    required this.enabled,
    required this.onTap,
  });

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: enabled ? onTap : null,
      style: AppTheme.actionButton(active: active).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(48, 44)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 6)),
        textStyle: WidgetStatePropertyAll(AppTheme.mono(12, weight: FontWeight.w600)),
      ),
      child: Text('$id'),
    );
  }
}

class _StopButton extends StatelessWidget {
  final bool enabled;
  final VoidCallback onTap;

  const _StopButton({required this.enabled, required this.onTap});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: enabled ? onTap : null,
      style: AppTheme.actionButton(danger: true).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(64, 44)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 6)),
      ),
      child: const Text('STOP'),
    );
  }
}
