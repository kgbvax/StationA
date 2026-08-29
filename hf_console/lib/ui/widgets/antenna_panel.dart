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
    // No state (or a state without 'selected') must not masquerade as a
    // port — least of all 'off', which would paint a dead bridge as a
    // deliberate grounded-safety state. It renders as Unknown instead.
    final selectedRaw = store.stateValueAs<String>('muehle/hf/ant-switch', 'selected');
    final selected = selectedRaw ?? '?';
    final settled = store.stateValueAs<bool>('muehle/hf/ant-switch', 'settled') ?? false;

    final selectSlot = store.slots['muehle/hf/antenna-select'];
    final selectOnline = selectSlot?.isOnline ?? false;
    // Cold-switch guard (model §6): a port moves only with RF inhibited AND
    // RX *confirmed* — unknown radio state must block, not allow. RF is
    // reported on three independent paths — the radio's tx bit, its tune
    // carrier, and the PA's own keyed telemetry — and the switch may only
    // move when the radio link is up and all of them say rx/idle. The
    // reconciler path is exempt: with antenna-select online it arbitrates
    // the RF-inhibit ordering itself.
    final radioSlot = store.slots['muehle/hf/radio'];
    final radioOnline = (radioSlot?.isOnline ?? false) &&
        (store.stateValueAs<bool>('muehle/hf/radio', 'device_online') ?? true);
    final radioTx = store.stateValueAs<String>('muehle/hf/radio', 'tx');
    final radioTuning = store.stateValueAs<bool>('muehle/hf/radio', 'tuning');
    final paKeyed = store.stateValueAs<String>('muehle/hf/pa', 'keyed');
    final rfOn = radioTx == 'tx' || radioTuning == true || paKeyed == 'tx';
    final rfSafe = radioOnline && radioTx == 'rx' && radioTuning != true && paKeyed != 'tx';
    // No fabricated 'auto': with the reconciler offline or absent (it may not
    // even be deployed), the operator drives the switch directly and the
    // header must say so instead of asserting a policy nobody enforces.
    final mode = store.stateValueAs<String>('muehle/hf/antenna-select', 'mode');
    final managed = selectOnline && mode != null;

    final antName = antennaMap[selected] ?? selected;

    final isManual = mode == 'manual';
    // 'off' = all antenna ports grounded: nothing is connected to the TX
    // path, so operating is impossible — the label must shout that in red.
    // Only a confirmed state may claim it; unknown must not.
    final grounded = selectedRaw == 'off';
    // Manual antenna-select also blocks operational routing; it is a
    // deliberate override state and gets the same red treatment.
    final blocked = grounded || isManual;
    // With the reconciler absent the operator taps drive ant-switch directly.
    final modeLabel = managed ? mode.toUpperCase() : 'DIRECT';

    void selectPort(String port) {
      if (!switchOnline) return;
      // In manual mode the operator drives the switch directly. In auto mode,
      // send a request to antenna-select if it is online; otherwise fall back
      // to driving the switch directly.
      final direct = isManual || !selectOnline;
      if (direct && !rfSafe) return; // hot-switch guard: fail closed
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
                  '$modeLabel · $antName',
                  style: AppTheme.mono(
                    12,
                    weight: FontWeight.w700,
                    color: blocked
                        ? AppTheme.red
                        : managed
                            ? settled
                                ? AppTheme.accent
                                : AppTheme.amber
                            : AppTheme.amber,
                  ),
                ),
                if (!settled) ...[
                  const SizedBox(width: 6),
                  // Moving relays pass no RF; make that explicit, not a
                  // blink-and-guess dot.
                  Text('NO RF', style: AppTheme.mono(10, color: AppTheme.amber, weight: FontWeight.w700)),
                  const SizedBox(width: 4),
                  _PendingDot(),
                ],
                if (rfOn) ...[
                  const SizedBox(width: 6),
                  Text('RF ON', style: AppTheme.mono(10, color: AppTheme.red, weight: FontWeight.w700)),
                ],
                if (!rfSafe && !rfOn && (isManual || !selectOnline)) ...[
                  const SizedBox(width: 6),
                  Text('RF ?', style: AppTheme.mono(10, color: AppTheme.amber, weight: FontWeight.w700)),
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
                final direct = isManual || !selectOnline;
                return ElevatedButton(
                  // Exactly selectPort's guard: direct-drive moves the port
                  // only with RF inhibited and RX confirmed (fail closed).
                  onPressed: switchOnline && (!direct || rfSafe) ? () => selectPort(port) : null,
                  // Grounded is the one selection that prevents operation,
                  // so even while active it renders in solid red, not accent.
                  style: AppTheme.actionButton(
                    dangerActive: isActive && grounded,
                    active: isActive && !grounded,
                  ),
                  child: Text(label.toUpperCase()),
                );
              }),
              const SizedBox(width: 16),
              ElevatedButton(
                onPressed: selectOnline ? () => setMode('auto') : null,
                style: AppTheme.actionButton(active: managed && mode == 'auto'),
                child: const Text('AUTO'),
              ),
              ElevatedButton(
                onPressed: selectOnline ? () => setMode('manual') : null,
                // Manual routing overrides the reconciler — shown in solid
                // red while engaged, like the grounded state.
                style: AppTheme.actionButton(dangerActive: isManual),
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
