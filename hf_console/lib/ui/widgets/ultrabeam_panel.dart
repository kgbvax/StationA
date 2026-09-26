import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'pulsing_amber_button.dart';
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

    // Smart rotation (beamsteer): the logger's rotate requests flip the
    // beam 180° when the station is behind, else rotate to the cheaper
    // lobe. The toggle is beamsteer's own retained /cmd — independent of
    // the controller link and of element travel.
    final steerSlot = store.slots['muehle/hf/beam-steer'];
    final steerOnline = (steerSlot?.isOnline ?? false) && store.linkUp;
    final smartOn = store.stateValueAs<bool>('muehle/hf/beam-steer', 'enabled') ?? false;
    final lastRaw = store.stateValue('muehle/hf/beam-steer', 'last');
    final smartReason = (steerOnline && smartOn && lastRaw is Map) ? lastRaw['reason'] as String? : null;

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
        border: Border(
          top: BorderSide(color: AppTheme.cardLine),
          bottom: BorderSide(color: AppTheme.cardLine),
        ),
      ),
      child: Stack(
        children: [
          Padding(
            // Top padding clears the corner-overlaid status pill — 32, not
            // the 26 other cards use, because RETRACT sits directly under it.
            padding: const EdgeInsets.fromLTRB(12, 32, 12, 10),
            // RETRACT is pinned to the right edge, apart from the direction
            // and mode buttons, so the emergency action is never reached by
            // a slip off a neighbouring button.
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Expanded(
                  child: Wrap(
                    spacing: 8,
                    runSpacing: 8,
                    crossAxisAlignment: WrapCrossAlignment.end,
                    children: [
                      // The three direction buttons share one width — sized for
                      // 'FORWARD' at the action-button style with the tablet's
                      // system font scale (88 dp wrapped at >1.0 scale). Wrap so
                      // AUTO drops to a second line on phone widths instead of
                      // overflowing the row.
                      _DirectionButton(
                        width: 112,
                        label: 'FORWARD',
                        active: direction == 'forward',
                        // Elements moving: lock taps so rapid presses can't queue
                        // competing direction cmds against mid-travel motors —
                        // the same lockout ultrabridge's own web UI applies.
                        onPressed: (online && !moving) ? () => send(antCtrlDirectionPayload('forward')) : null,
                      ),
                      _DirectionButton(
                        width: 112,
                        label: '180°',
                        active: direction == 'reverse',
                        // Reverse is a deliberate but irregular state for the
                        // Ultrabeam — amber pulse while engaged, not the cyan
                        // "normal" highlight and not red error chrome.
                        irregular: true,
                        onPressed: (online && !moving && !on6m) ? () => send(antCtrlDirectionPayload('reverse')) : null,
                      ),
                      _DirectionButton(
                        width: 112,
                        label: 'BI-DIR',
                        active: direction == 'bidirectional',
                        onPressed: (online && !moving && !on6m)
                            ? () => send(antCtrlDirectionPayload('bidirectional'))
                            : null,
                      ),
                      // AUTO (smart rotation) is a mode, not a direction: set apart from the
                      // direction group like AUTO/MANUAL on the Ant switch row.
                      const SizedBox(width: 12),
                      _DirectionButton(
                        width: 112,
                        label: 'AUTO',
                        active: smartOn,
                        onPressed: steerOnline
                            ? () => mqtt.publish(
                                cmdTopic('hf/beam-steer'),
                                beamSteerEnablePayload(!smartOn),
                                retain: cmdRetain['muehle/hf/beam-steer']!,
                              )
                            : null,
                      ),
                    ],
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
          // The last smart-rotation decision rides in the pill's band, left
          // of the pill: it comes and goes with every logger request, so it
          // must never change the card's height (that shifted the compass).
          Positioned(
            top: 6,
            left: 12,
            right: 10,
            child: Row(
              children: [
                Expanded(
                  child: smartReason == null || smartReason.isEmpty
                      ? const SizedBox.shrink()
                      : Text(
                          'AUTO · $smartReason',
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: AppTheme.mono(11, color: AppTheme.txtMute),
                        ),
                ),
                const SizedBox(width: 8),
                StatusPill(
                  slots: const ['muehle/hf/ant-ctrl'],
                  label: 'Ultrabeam',
                  info: pillBand.isEmpty ? null : pillBand,
                  suffix: suffix.isEmpty ? null : suffix,
                  suffixColor: suffixColor,
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

class _DirectionButton extends StatelessWidget {
  final String label;
  final double width;
  final bool active;

  /// Irregular-but-deliberate state (180° on the Ultrabeam): amber pulse
  /// while engaged instead of the cyan "normal" highlight.
  final bool irregular;
  final VoidCallback? onPressed;

  const _DirectionButton({
    required this.label,
    required this.width,
    required this.active,
    this.irregular = false,
    this.onPressed,
  });

  @override
  Widget build(BuildContext context) {
    final child = Text(label, maxLines: 1, softWrap: false);
    return SizedBox(
      width: width,
      child: irregular
          ? PulsingAmberButton(engaged: active, onPressed: onPressed, child: child)
          : ElevatedButton(
              onPressed: onPressed,
              style: AppTheme.actionButton(active: active),
              child: child,
            ),
    );
  }
}
