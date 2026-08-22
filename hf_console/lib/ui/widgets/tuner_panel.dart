import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class TunerPanel extends StatelessWidget {
  const TunerPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/tuner'];
    final online = slot?.isOnline ?? false;
    final inline = store.stateValueAs<bool>('muehle/hf/tuner', 'inline') ?? false;
    final settling = store.stateValueAs<bool>('muehle/hf/tuner', 'settling') ?? false;
    final fault = store.stateValueAs<String>('muehle/hf/tuner', 'fault') ?? '';
    final swr = store.stateValueAs<num>('muehle/hf/tuner', 'swr')?.toDouble() ?? 1.0;

    final tagLabel = _tunerTag(inline, settling, fault, swr, online);
    final tagColor = !online
        ? AppTheme.txtMute
        : fault.isNotEmpty
            ? AppTheme.red
            : settling
                ? AppTheme.amber
                : inline
                    ? AppTheme.green
                    : AppTheme.txtMute;

    void setInline(bool value) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/tuner'),
        tunerInlinePayload(value),
        retain: cmdRetain['muehle/hf/tuner']!,
      );
    }

    void tune(String mode) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/tuner'),
        tunerTunePayload(mode),
        retain: cmdRetain['muehle/hf/tuner']!,
      );
    }

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          CardHeader(
            title: 'Tuner · ATR-1000',
            trailing: _Tag(tagLabel, tagColor),
          ),
          const SizedBox(height: 10),
          Row(
            children: [
              Expanded(child: ElevatedButton(
                onPressed: online ? () => setInline(false) : null,
                style: AppTheme.actionButton(amber: true, active: !inline),
                child: const Text('BYPASS'),
              )),
              const SizedBox(width: 5),
              Expanded(child: ElevatedButton(
                onPressed: online ? () => tune('mem') : null,
                style: AppTheme.actionButton(),
                child: const Text('TUNE MEM'),
              )),
              const SizedBox(width: 5),
              Expanded(child: ElevatedButton(
                onPressed: online ? () => tune('full') : null,
                style: AppTheme.actionButton(),
                child: const Text('TUNE FULL'),
              )),
            ],
          ),
        ],
      ),
    );
  }

  String _tunerTag(bool inline, bool settling, String fault, double swr, bool online) {
    if (!online) return 'OFFLINE';
    if (fault.isNotEmpty) return fault.toUpperCase();
    if (settling) return 'TUNING';
    if (inline) return 'IN LINE · SWR ${swr.toStringAsFixed(swr < 10 ? 1 : 0)}';
    return 'BYPASS';
  }
}

class _Tag extends StatelessWidget {
  final String label;
  final Color color;

  const _Tag(this.label, this.color);

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(label, style: AppTheme.mono(11, color: color, weight: FontWeight.w700)),
    );
  }
}
