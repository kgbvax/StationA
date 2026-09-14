import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'status_pill.dart';

class TunerPanel extends StatelessWidget {
  const TunerPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/tuner'];
    final online = (slot?.isOnline ?? false) && store.linkUp;
    final inline = store.stateValueAs<bool>('muehle/hf/tuner', 'inline') ?? false;
    final settling = store.stateValueAs<bool>('muehle/hf/tuner', 'settling') ?? false;
    final fault = store.stateValueAs<String>('muehle/hf/tuner', 'fault') ?? '';
    final swr = store.stateValueAs<num>('muehle/hf/tuner', 'swr')?.toDouble() ?? 1.0;

    final (suffix, suffixColor) =
        _tunerState(inline, settling, fault, swr) ?? ('', null);

    void setInline(bool value) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/tuner'),
        tunerInlinePayload(value),
        retain: cmdRetain['muehle/hf/tuner']!,
      );
    }

    void tune(String mode) {
      // A second tune queued against a settling tuner just competes with the
      // in-flight one — hold taps while it works.
      if (!online || settling) return;
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
            trailing: StatusPill(
              slots: const ['muehle/hf/tuner'],
              label: 'ATR-1000',
              suffix: suffix.isEmpty ? null : suffix,
              suffixColor: suffixColor,
            ),
          ),
          const SizedBox(height: 10),
          Row(
            children: [
              Expanded(child: ElevatedButton(
                onPressed: online ? () => setInline(false) : null,
                style: AppTheme.actionButton(amber: true, active: !inline),
                child: const Text('BYPASS'),
              )),
              const SizedBox(width: 8),
              Expanded(child: ElevatedButton(
                onPressed: online && !settling ? () => tune('mem') : null,
                style: AppTheme.actionButton(),
                child: Text(settling ? 'TUNING…' : 'TUNE MEM'),
              )),
              const SizedBox(width: 8),
              Expanded(child: ElevatedButton(
                onPressed: online && !settling ? () => tune('full') : null,
                style: AppTheme.actionButton(),
                child: Text(settling ? 'TUNING…' : 'TUNE FULL'),
              )),
            ],
          ),
        ],
      ),
    );
  }

  /// Irregular states only — an in-line tuner with healthy SWR is the plain
  /// green device name on the pill. Offline is the pill's own concern.
  (String, Color)? _tunerState(bool inline, bool settling, String fault, double swr) {
    if (fault.isNotEmpty) return (fault.toUpperCase(), AppTheme.red);
    if (settling) return ('TUNING', AppTheme.amber);
    if (!inline) return ('BYPASS', AppTheme.amber); // degraded TX path
    if (swr >= 2.0) return ('SWR ${swr.toStringAsFixed(swr < 10 ? 1 : 0)}', _swrColor(swr));
    return null;
  }

  /// SWR thresholds on the inline suffix — 3.5:1 and 1.1:1 must not read alike.
  Color _swrColor(double swr) {
    if (swr >= 3.0) return AppTheme.red;
    return AppTheme.amber;
  }
}
