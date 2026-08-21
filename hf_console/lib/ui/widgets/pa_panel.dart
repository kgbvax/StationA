import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class PaPanel extends StatelessWidget {
  const PaPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/pa'];
    final online = slot?.isOnline ?? false;
    final mode = store.stateValueAs<String>('muehle/hf/pa', 'mode') ?? 'standby';
    final keyed = store.stateValueAs<String>('muehle/hf/pa', 'keyed') ?? 'rx';
    final fault = store.stateValueAs<String>('muehle/hf/pa', 'fault') ?? 'none';
    final error = store.stateValueAs<String>('muehle/hf/pa', 'error') ?? '';
    final temp = store.stateValueAs<num>('muehle/hf/pa', 'temp_c')?.toDouble() ?? 0.0;
    final fwd = store.stateValueAs<num>('muehle/hf/pa', 'fwd_power_w')?.toDouble() ?? 0.0;
    final rfl = store.stateValueAs<num>('muehle/hf/pa', 'rfl_power_w')?.toDouble() ?? 0.0;
    final swr = store.stateValueAs<num>('muehle/hf/pa', 'swr')?.toDouble() ?? 1.0;

    final (tagLabel, tagColor) = _paTag(mode, keyed, fault, error, temp, online);

    void setMode(String value) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/pa'),
        paSetModePayload(value),
        retain: cmdRetain['muehle/hf/pa']!,
      );
    }

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          CardHeader(
            title: 'PA · ACOM 1200S',
            trailing: _Tag(tagLabel, tagColor),
          ),
          IntrinsicHeight(
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    children: [
                      _Meter(
                        value: fwd,
                        max: 1200,
                        unit: 'W FWD',
                        labels: const ['0', '500', '1000', '1200'],
                        fillColor: AppTheme.green,
                        compact: true,
                      ),
                      _Meter(
                        value: swr,
                        max: 4.0,
                        unit: 'SWR',
                        labels: const ['1.0', '1.5', '3.0', '4.0'],
                        fillColor: AppTheme.amber,
                        compact: true,
                      ),
                      if (rfl > 0)
                        Padding(
                          padding: const EdgeInsets.only(top: 2),
                          child: Text('REFL ${rfl.toStringAsFixed(0)} W', style: AppTheme.mono(11, color: AppTheme.txtFaint)),
                        ),
                    ],
                  ),
                ),
                const SizedBox(width: 10),
                SizedBox(
                  width: 96,
                  child: Column(
                    mainAxisAlignment: MainAxisAlignment.spaceBetween,
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: [
                      SizedBox(
                        height: 34,
                        child: ElevatedButton(
                          onPressed: online ? () => setMode('operate') : null,
                          style: AppTheme.actionButton(active: mode == 'operate'),
                          child: const Text('OPERATE'),
                        ),
                      ),
                      SizedBox(
                        height: 34,
                        child: ElevatedButton(
                          onPressed: online ? () => setMode('standby') : null,
                          style: AppTheme.actionButton(amber: true, active: mode == 'standby'),
                          child: const Text('STANDBY'),
                        ),
                      ),
                    ],
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  (String, Color) _paTag(String mode, String keyed, String fault, String error, double temp, bool online) {
    if (!online) return ('OFFLINE', AppTheme.txtMute);
    if (fault.isNotEmpty && fault != 'none') {
      final label = error.isNotEmpty ? error.toUpperCase() : fault.toUpperCase();
      return (label, AppTheme.red);
    }
    if (keyed == 'tx') return ('● TX', AppTheme.red);
    if (keyed == 'inhibited') return ('INHIBITED', AppTheme.amber);
    final tempLabel = temp > 0 ? ' · ${temp.round()} °C' : '';
    if (mode == 'operate') return ('OPERATE$tempLabel', AppTheme.green);
    return ('STANDBY$tempLabel', AppTheme.amber);
  }
}

class _Meter extends StatelessWidget {
  final double value;
  final double max;
  final String unit;
  final List<String> labels;
  final Color fillColor;
  final bool compact;

  const _Meter({
    required this.value,
    required this.max,
    required this.unit,
    required this.labels,
    required this.fillColor,
    this.compact = false,
  });

  @override
  Widget build(BuildContext context) {
    final fraction = (value / max).clamp(0.0, 1.0);
    final valueStyle = AppTheme.mono(compact ? 18 : 24, weight: FontWeight.w700);
    final unitStyle = AppTheme.mono(compact ? 11 : 13, color: AppTheme.txtFaint);
    final labelStyle = AppTheme.mono(compact ? 9 : 11, color: AppTheme.txtFaint);
    final barHeight = compact ? 8.0 : 12.0;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Text.rich(
          TextSpan(
            children: [
              TextSpan(text: value.toStringAsFixed(value < 10 ? 1 : 0), style: valueStyle),
              TextSpan(text: ' $unit', style: unitStyle),
            ],
          ),
        ),
        SizedBox(height: compact ? 2 : 3),
        SizedBox(
          height: barHeight,
          child: Stack(
            children: [
              Container(decoration: BoxDecoration(color: AppTheme.pane, borderRadius: BorderRadius.circular(4), border: Border.all(color: AppTheme.cardLine))),
              FractionallySizedBox(
                alignment: Alignment.centerLeft,
                widthFactor: fraction,
                child: Container(
                  decoration: BoxDecoration(
                    gradient: LinearGradient(colors: [fillColor, fillColor, AppTheme.orange]),
                    borderRadius: BorderRadius.circular(4),
                  ),
                ),
              ),
            ],
          ),
        ),
        SizedBox(height: compact ? 2 : 3),
        Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: labels.map((l) => Text(l, style: labelStyle)).toList(),
        ),
      ],
    );
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
