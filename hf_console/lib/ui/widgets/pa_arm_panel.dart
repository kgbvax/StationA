import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class PaArmPanel extends StatelessWidget {
  const PaArmPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/pa-arm'];
    final online = slot?.isOnline ?? false;
    final enabled = store.stateValueAs<bool>('muehle/hf/pa-arm', 'enabled') ?? false;
    final armed = store.stateValueAs<bool>('muehle/hf/pa-arm', 'armed') ?? false;
    final error = store.stateValueAs<String>('muehle/hf/pa-arm', 'error') ?? '';

    void setEnabled(bool value) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/pa-arm'),
        paArmPayload(value),
        retain: cmdRetain['muehle/hf/pa-arm']!,
      );
    }

    final (label, color) = _armTag(online, enabled, armed, error);

    return CardContainer(
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text('PA ARM'.toUpperCase(), style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
          Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              Container(
                padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
                decoration: BoxDecoration(
                  color: AppTheme.blend(color, 0.12),
                  border: Border.all(color: color),
                  borderRadius: BorderRadius.circular(4),
                ),
                child: Text(label, style: AppTheme.mono(11, color: color, weight: FontWeight.w700)),
              ),
              const SizedBox(width: 8),
              ElevatedButton(
                onPressed: online ? () => setEnabled(!enabled) : null,
                style: AppTheme.actionButton(active: enabled, danger: !enabled),
                child: Text(enabled ? 'SAFE' : 'ARM'),
              ),
            ],
          ),
        ],
      ),
    );
  }

  (String, Color) _armTag(bool online, bool enabled, bool armed, String error) {
    if (!online) return ('OFFLINE', AppTheme.txtMute);
    if (error.isNotEmpty) return (error.toUpperCase(), AppTheme.red);
    if (armed) return ('ARMED', AppTheme.red);
    if (enabled) return ('ENABLED', AppTheme.amber);
    return ('SAFE', AppTheme.green);
  }
}
