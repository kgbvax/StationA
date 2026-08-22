import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class AntennaPanel extends StatelessWidget {
  const AntennaPanel({super.key});

  static const _ports = ['off', 'port1', 'port4', 'port5', 'port6'];

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final switchSlot = store.slots['muehle/hf/ant-switch'];
    final switchOnline = switchSlot?.isOnline ?? false;
    final selected = store.stateValueAs<String>('muehle/hf/ant-switch', 'selected') ?? 'off';
    final settled = store.stateValueAs<bool>('muehle/hf/ant-switch', 'settled') ?? false;

    final selectSlot = store.slots['muehle/hf/antenna-select'];
    final selectOnline = selectSlot?.isOnline ?? false;
    final mode = store.stateValueAs<String>('muehle/hf/antenna-select', 'mode') ?? 'auto';

    final antName = antennaMap[selected] ?? selected;

    final isManual = mode == 'manual';

    void selectPort(String port) {
      if (!switchOnline) return;
      // In manual mode the operator drives the switch directly. In auto mode,
      // send a request to antenna-select if it is online; otherwise fall back
      // to driving the switch directly.
      if (isManual) {
        mqtt.publish(
          cmdTopic('hf/ant-switch'),
          antennaSwitchPayload(port),
          retain: cmdRetain['muehle/hf/ant-switch']!,
        );
      } else if (selectOnline) {
        mqtt.publish(
          cmdTopic('hf/antenna-select'),
          antennaSelectPayload(port),
          retain: cmdRetain['muehle/hf/antenna-select']!,
        );
      } else {
        mqtt.publish(
          cmdTopic('hf/ant-switch'),
          antennaSwitchPayload(port),
          retain: cmdRetain['muehle/hf/ant-switch']!,
        );
      }
    }

    void setMode(String newMode) {
      if (!selectOnline) return;
      mqtt.publish(
        cmdTopic('hf/antenna-select'),
        antennaSelectPayload(newMode),
        retain: cmdRetain['muehle/hf/antenna-select']!,
      );
    }

    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'Routing',
            trailing: Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                Text(
                  '${mode.toUpperCase()} · $antName',
                  style: AppTheme.mono(12, weight: FontWeight.w700, color: settled ? AppTheme.accent : AppTheme.amber),
                ),
                if (!settled) ...[
                  const SizedBox(width: 6),
                  _PendingDot(),
                ],
              ],
            ),
          ),
          const SizedBox(height: 6),
          Wrap(
            spacing: 4,
            runSpacing: 4,
            children: [
              ..._ports.map((port) {
                final label = antennaMap[port] ?? port;
                final isActive = port == selected;
                return ElevatedButton(
                  onPressed: switchOnline ? () => selectPort(port) : null,
                  style: AppTheme.actionButton(active: isActive),
                  child: Text(label.toUpperCase()),
                );
              }),
              const SizedBox(width: 16),
              ElevatedButton(
                onPressed: selectOnline ? () => setMode('auto') : null,
                style: AppTheme.actionButton(active: mode == 'auto'),
                child: const Text('AUTO'),
              ),
              ElevatedButton(
                onPressed: selectOnline ? () => setMode('manual') : null,
                style: AppTheme.actionButton(active: mode == 'manual'),
                child: const Text('MANUAL'),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _PendingDot extends StatefulWidget {
  @override
  State<_PendingDot> createState() => _PendingDotState();
}

class _PendingDotState extends State<_PendingDot> with SingleTickerProviderStateMixin {
  late final AnimationController _ctrl;

  @override
  void initState() {
    super.initState();
    _ctrl = AnimationController(vsync: this, duration: const Duration(milliseconds: 700))..repeat(reverse: true);
  }

  @override
  void dispose() {
    _ctrl.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AnimatedBuilder(
      animation: _ctrl,
      builder: (context, child) {
        return Container(
          width: 8,
          height: 8,
          decoration: BoxDecoration(
            color: AppTheme.blend(AppTheme.amber, 0.4 + _ctrl.value * 0.4),
            shape: BoxShape.circle,
          ),
        );
      },
    );
  }
}
