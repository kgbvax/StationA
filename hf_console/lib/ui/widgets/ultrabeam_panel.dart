import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'status_pill.dart';

class UltrabeamPanel extends StatefulWidget {
  const UltrabeamPanel({super.key});

  @override
  State<UltrabeamPanel> createState() => _UltrabeamPanelState();
}

class _UltrabeamPanelState extends State<UltrabeamPanel> {
  // Set once we've pushed the forced-forward correction for the current 6m
  // state, so we don't re-publish on every rebuild while the controller's
  // direction state catches up. Clears as soon as the invalid state resolves.
  bool _forwardForced = false;

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/ant-ctrl'];
    final online = (slot?.isOnline ?? false) && store.linkUp;
    final direction = store.stateValueAs<String>('muehle/hf/ant-ctrl', 'direction') ?? 'forward';
    final moving = store.stateValueAs<bool>('muehle/hf/ant-ctrl', 'moving') ?? false;
    final radioBand = store.stateValueAs<String>('muehle/hf/radio', 'band') ?? '';
    // The controller's own tuned band can lag the radio after a QSY with the
    // controller link flaky or a failed freq cmd — operating then means
    // keying into a mis-tuned beam. Unknown/empty is reported as unknown, not
    // as a mismatch, so pre-first-state silence doesn't cry wolf.
    final ctrlBand = store.stateValueAs<String>('muehle/hf/ant-ctrl', 'band') ?? '';
    // The two bridges label out-of-allocation frequencies differently
    // (flexbridge: 'gen'/'unknown', ultrabridge: 'band-<n>') and there they
    // can agree on frequency while disagreeing on label — don't cry wolf.
    bool comparable(String b) => b.isNotEmpty && !{'gen', 'unknown'}.contains(b) && !b.startsWith('band-');
    final bandMismatch = online && comparable(ctrlBand) && comparable(radioBand) && ctrlBand != radioBand;

    // 6m is the only band where the Ultrabeam's elements support just the
    // forward direction — 180° and bi-dir don't exist there. Force the
    // controller back to forward (once per invalid state) and grey the
    // invalid direction buttons until the radio leaves 6m. The correction
    // obeys the same while-moving lockout as the manual buttons — a queued
    // direction cmd is a queued direction cmd either way — and re-fires
    // once travel ends because the flag resets while moving.
    final on6m = radioBand == '6m';
    final needsForward = on6m && online && direction != 'forward' && !moving;
    if (needsForward && !_forwardForced) {
      _forwardForced = true;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) {
          mqtt.publish(
            cmdTopic('hf/ant-ctrl'),
            antCtrlDirectionPayload('forward'),
            retain: cmdRetain['muehle/hf/ant-ctrl']!,
          );
        }
      });
    } else if (!needsForward || moving) {
      _forwardForced = false;
    }

    void send(String cmd) {
      if (!online) return;
      mqtt.publish(cmdTopic('hf/ant-ctrl'), cmd, retain: cmdRetain['muehle/hf/ant-ctrl']!);
    }

    final (suffix, suffixColor) = moving
        ? ('MOVING', AppTheme.red)
        : bandMismatch
            ? ('BAND MISMATCH', AppTheme.red)
            : ('', null);

    // The controller's tuned band, for the pill's regular (green) info
    // segment. Out-of-allocation labels ('band-<n>', 'unknown') and a
    // pre-first-state empty read as nothing rather than as noise.
    final bandRegExp = RegExp(r'^\d+m$');
    final pillBand = switch (ctrlBand) {
      String b when bandRegExp.hasMatch(b) => b,
      'gen' => 'gen',
      _ => '',
    };

    // The card is the outer Container so it always fills the layout width;
    // the pill floats over its top padding band. (A Stack whose children
    // are all Positioned cannot size itself inside a scroll view.)
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      child: Stack(
        children: [
          Padding(
            // Top padding clears the corner-overlaid status pill.
            padding: const EdgeInsets.fromLTRB(12, 26, 12, 10),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.end,
              children: [
                // The three direction buttons share the free width equally —
                // intrinsic sizing made '180°' a sliver next to 'FORWARD'.
                Expanded(
                  child: _DirectionButton(
                    label: 'FORWARD',
                    active: direction == 'forward',
                    // Elements moving: lock taps so rapid presses can't queue
                    // competing direction cmds against mid-travel motors —
                    // the same lockout ultrabridge's own web UI applies.
                    onPressed: (online && !moving) ? () => send(antCtrlDirectionPayload('forward')) : null,
                  ),
                ),
                const SizedBox(width: 8),
                Expanded(
                  child: _DirectionButton(
                    label: '180°',
                    active: direction == 'reverse',
                    onPressed: (online && !moving && !on6m) ? () => send(antCtrlDirectionPayload('reverse')) : null,
                  ),
                ),
                const SizedBox(width: 8),
                Expanded(
                  child: _DirectionButton(
                    label: 'BI-DIR',
                    active: direction == 'bidirectional',
                    onPressed: (online && !moving && !on6m) ? () => send(antCtrlDirectionPayload('bidirectional')) : null,
                  ),
                ),
                const SizedBox(width: 8),
                ElevatedButton(
                  // RETRACT stays pressable while moving — it is the emergency
                  // action for an unexpected or stuck direction state, and
                  // ultrabridge (web UI and handlers alike) keeps it available
                  // during travel deliberately.
                  onPressed: online ? () => send(antCtrlRetractPayload()) : null,
                  style: AppTheme.actionButton(danger: true),
                  child: const Text('RETRACT'),
                ),
              ],
            ),
          ),
          // The pill paints last so it sits on top of the card.
          Positioned(
            top: 6,
            right: 10,
            child: StatusPill(
              slots: const ['muehle/hf/ant-ctrl'],
              label: 'Ultrabeam',
              info: pillBand.isEmpty ? null : pillBand,
              suffix: suffix.isEmpty ? null : suffix,
              suffixColor: suffixColor,
            ),
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
