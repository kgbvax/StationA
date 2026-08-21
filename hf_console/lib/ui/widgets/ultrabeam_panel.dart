import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class UltrabeamPanel extends StatelessWidget {
  const UltrabeamPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/ant-ctrl'];
    final online = slot?.isOnline ?? false;
    final direction = store.stateValueAs<String>('muehle/hf/ant-ctrl', 'direction') ?? 'forward';
    final moving = store.stateValueAs<bool>('muehle/hf/ant-ctrl', 'moving') ?? false;

    void send(String cmd) {
      if (!online) return;
      mqtt.publish(cmdTopic('hf/ant-ctrl'), cmd, retain: cmdRetain['muehle/hf/ant-ctrl']!);
    }

    final (pillLabel, pillColor) = online
        ? (moving ? 'MOVING' : direction.toUpperCase(), moving ? AppTheme.red : AppTheme.accent)
        : ('OFFLINE', AppTheme.red);

    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'Ultrabeam',
            trailing: Container(
              padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
              decoration: BoxDecoration(
                color: AppTheme.blend(pillColor, 0.12),
                border: Border.all(color: pillColor),
                borderRadius: BorderRadius.circular(4),
              ),
              child: Text(
                pillLabel,
                style: AppTheme.mono(12, weight: FontWeight.w700, color: pillColor),
              ),
            ),
          ),
          const SizedBox(height: 6),
          Wrap(
            spacing: 4,
            runSpacing: 4,
            children: [
              _DirectionButton(
                label: 'FORWARD',
                active: direction == 'forward',
                onPressed: online ? () => send(antCtrlDirectionPayload('forward')) : null,
              ),
              _DirectionButton(
                label: '180°',
                active: direction == 'reverse',
                onPressed: online ? () => send(antCtrlDirectionPayload('reverse')) : null,
              ),
              _DirectionButton(
                label: 'BI-DIR',
                active: direction == 'bidirectional',
                onPressed: online ? () => send(antCtrlDirectionPayload('bidirectional')) : null,
              ),
              ElevatedButton(
                onPressed: online ? () => send(antCtrlRetractPayload()) : null,
                style: AppTheme.actionButton(danger: true),
                child: const Text('RETRACT'),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _DirectionButton extends StatelessWidget {
  final String label;
  final bool active;
  final VoidCallback? onPressed;

  const _DirectionButton({required this.label, required this.active, this.onPressed});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: onPressed,
      style: AppTheme.actionButton(active: active),
      child: Text(label),
    );
  }
}
