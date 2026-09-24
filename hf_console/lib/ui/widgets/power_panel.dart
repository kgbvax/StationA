import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'status_pill.dart';

class PowerPanel extends StatelessWidget {
  const PowerPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final master = store.slots['muehle/power/master'];
    final masterOn = (store.stateValueAs<String>('muehle/power/master', 'power') ?? 'off') == 'on';
    final masterOnline = (master?.isOnline ?? false) && store.linkUp;

    final psu = store.slots['muehle/power/psu-13v8'];
    final psuOn = (store.stateValueAs<String>('muehle/power/psu-13v8', 'power') ?? 'off') == 'on';
    final psuOnline = (psu?.isOnline ?? false) && store.linkUp;

    final sw = store.slots['muehle/hf/switch'];
    final paOn = (store.stateValueAs<String>('muehle/hf/switch', 'pa') ?? 'off') == 'on';
    final trxOn = (store.stateValueAs<String>('muehle/hf/switch', 'trx') ?? 'off') == 'on';
    final switchOnline = (sw?.isOnline ?? false) && store.linkUp;

    final seq = store.slots['muehle/hf/power-seq'];
    final seqOnline = (seq?.isOnline ?? false) && store.linkUp;
    final seqPhase = store.stateValueAs<String>('muehle/hf/power-seq', 'phase') ?? 'idle';
    final seqFault = store.stateValueAs<String>('muehle/hf/power-seq', 'fault') ?? '';
    // The step within starting/stopping (e.g. `psu-on`); "" at idle/running.
    final seqStep = store.stateValueAs<String>('muehle/hf/power-seq', 'step') ?? '';

    void setMaster(bool on) {
      if (!masterOnline) return;
      mqtt.publish(
        cmdTopic('power/master'),
        powerSetPayload(on ? 'on' : 'off'),
        retain: cmdRetain['muehle/power/master']!,
      );
    }

    void setPsu(bool on) {
      if (!psuOnline) return;
      mqtt.publish(
        cmdTopic('power/psu-13v8'),
        powerSetPayload(on ? 'on' : 'off'),
        retain: cmdRetain['muehle/power/psu-13v8']!,
      );
    }

    void setTrx(bool on) {
      if (!switchOnline) return;
      mqtt.publish(
        cmdTopic('hf/switch'),
        switchSetTrxPayload(on ? 'on' : 'off'),
        retain: cmdRetain['muehle/hf/switch']!,
      );
    }

    void setPa(bool on) {
      if (!switchOnline) return;
      mqtt.publish(
        cmdTopic('hf/switch'),
        switchSetPaPayload(on ? 'on' : 'off'),
        retain: cmdRetain['muehle/hf/switch']!,
      );
    }

    void stopStation() {
      mqtt.publish(
        cmdTopic('hf/power-seq'),
        powerSeqStopPayload(),
        retain: cmdRetain['muehle/hf/power-seq']!,
      );
    }

    void startStation() {
      mqtt.publish(
        cmdTopic('hf/power-seq'),
        powerSeqStartPayload(),
        retain: cmdRetain['muehle/hf/power-seq']!,
      );
    }

    final seqRunning = const {'running', 'starting'}.contains(seqPhase);

    final seqColor = seqFault.isNotEmpty
        ? AppTheme.red
        : const {'running': true, 'starting': true}.containsKey(seqPhase)
            ? AppTheme.green
            : AppTheme.txtMute;
    final seqLabel = seqFault.isNotEmpty
        ? 'FAULT'
        : const {'running': 'ON', 'starting': 'STARTING', 'stopping': 'STOPPING'}[seqPhase] ?? 'IDLE';

    final (pillSuffix, pillColor) = seqFault.isNotEmpty
        ? ('FAULT', AppTheme.red)
        : const {'starting', 'stopping'}.contains(seqPhase)
            ? (seqLabel, AppTheme.amber)
            : ('', null);

    // The card is the outer Container so it always fills the layout width;
    // the pill floats over its top padding band. (A Stack whose children
    // are all Positioned cannot size itself inside a scroll view.)
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border.all(color: AppTheme.blend(AppTheme.purpleBorder, 0.45)),
        borderRadius: BorderRadius.circular(6),
      ),
      child: Stack(
        children: [
          Padding(
            // Top padding clears the corner-overlaid status pill.
            padding: const EdgeInsets.fromLTRB(12, 26, 12, 10),
            // Two rows: the sequencer (START/STOP + phase readout), then all
            // four relays. The relays used to share one row with a sideways
            // scroll that hid TRX and PA off-screen on the tablet.
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  crossAxisAlignment: CrossAxisAlignment.center,
                  children: [
                    ElevatedButton(
                      onPressed: seqOnline && !seqRunning ? startStation : null,
                      style: AppTheme.actionButton().copyWith(
                        backgroundColor: const WidgetStatePropertyAll(Color(0x1E5CCB8A)),
                        foregroundColor: WidgetStatePropertyAll(AppTheme.green),
                        side: WidgetStatePropertyAll(BorderSide(color: AppTheme.green)),
                        minimumSize: const WidgetStatePropertyAll(Size(76, 57)),
                        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 10, vertical: 10)),
                      ),
                      child: const Text('START\nSTATION'),
                    ),
                    const SizedBox(width: 8),
                    ElevatedButton(
                      onPressed: seqOnline ? stopStation : null,
                      style: AppTheme.actionButton(danger: true).copyWith(
                        minimumSize: const WidgetStatePropertyAll(Size(76, 57)),
                        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 10, vertical: 10)),
                      ),
                      child: const Text('STOP\nSTATION'),
                    ),
                    const SizedBox(width: 16),
                    Flexible(
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Text('SEQUENCE', style: AppTheme.mono(10, color: AppTheme.txtFaint, letterSpacing: 0.12)),
                          Text(seqLabel, style: AppTheme.mono(14, color: seqColor, weight: FontWeight.w700)),
                          if (seqFault.isEmpty && const {'starting', 'stopping'}.contains(seqPhase) && seqStep.isNotEmpty)
                            Text(seqStep,
                                key: const ValueKey('seq-step'),
                                overflow: TextOverflow.ellipsis,
                                style: AppTheme.mono(11, color: AppTheme.txtMute)),
                        ],
                      ),
                    ),
                  ],
                ),
                const SizedBox(height: 10),
                Wrap(
                  spacing: 8,
                  runSpacing: 8,
                  children: [
                    _Relay(name: 'MAINS', on: masterOn, online: masterOnline, onToggle: setMaster),
                    _Relay(name: 'PSU 13.8V', on: psuOn, online: psuOnline, onToggle: setPsu),
                    _Relay(name: 'TRX', on: trxOn, online: switchOnline, onToggle: setTrx),
                    _Relay(name: 'PA', on: paOn, online: switchOnline, onToggle: setPa),
                  ],
                ),
              ],
            ),
          ),
          // The pill paints last so it sits on top of the card.
          Positioned(
            top: 6,
            right: 10,
            // Worst case across everything this module switches and sequences.
            child: StatusPill(
              slots: const [
                'muehle/power/master',
                'muehle/power/psu-13v8',
                'muehle/hf/switch',
                'muehle/hf/power-seq',
              ],
              label: 'Power',
              useMetaName: false,
              suffix: pillSuffix.isEmpty ? null : pillSuffix,
              suffixColor: pillColor,
              stickySuffix: true, // sequencer FAULT survives a dead link
            ),
          ),
        ],
      ),
    );
  }
}

class _Relay extends StatelessWidget {
  final String name;
  final bool on;
  final bool online;
  final ValueChanged<bool> onToggle;

  const _Relay({required this.name, required this.on, required this.online, required this.onToggle});

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      onTap: online ? () => onToggle(!on) : null,
      // A disabled relay must explain itself — a silent dim could be
      // deliberate-off or a dead bridge.
      child: Tooltip(
        message: online ? '' : '$name offline — relay uncontrollable',
        child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: AppTheme.pane,
          border: Border.all(color: AppTheme.cardLine),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(name, style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.06, color: AppTheme.txtMute)),
            const SizedBox(width: 8),
            Container(
              width: 34,
              height: 18,
              decoration: BoxDecoration(
                color: on && online ? AppTheme.blend(AppTheme.green, 0.20) : AppTheme.cardLine,
                borderRadius: BorderRadius.circular(9),
              ),
              child: AnimatedAlign(
                alignment: on ? Alignment.centerRight : Alignment.centerLeft,
                duration: const Duration(milliseconds: 120),
                child: Padding(
                  padding: const EdgeInsets.all(2),
                  child: Container(
                    width: 14,
                    height: 14,
                    decoration: BoxDecoration(
                      color: on && online ? AppTheme.green : online ? AppTheme.txtMute : AppTheme.txtFaint,
                      borderRadius: BorderRadius.circular(7),
                    ),
                  ),
                ),
              ),
            ),
            const SizedBox(width: 8),
            Text(
              online ? (on ? 'ON' : 'OFF') : 'OFFLINE',
              style: AppTheme.mono(11, color: online ? (on ? AppTheme.green : AppTheme.txtFaint) : AppTheme.red, weight: FontWeight.w700),
            ),
          ],
        ),
      ),
      ),
    );
  }
}
