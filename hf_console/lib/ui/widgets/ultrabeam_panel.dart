import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

/// Horst-Kevin — band-heckling dragon. Bundled from horstreporter's
/// `static/hk.jpg`; the PNG variant (`assets/img/hk-removebg.png`) is the
/// same photo with its background removed so it composites cleanly against
/// the card chrome. The 56 dp circle-clip + accent ring used in the first
/// pass clipped the dragon's horns; the alpha-clean PNG lets us render him
/// as-is, so the widget just paints the image with a tooltip.
class HorstKevin extends StatelessWidget {
  /// Target rendered height in logical pixels. Width is computed from the
  /// source's 482:517 aspect ratio (≈ 0.93) so the dragon stays proportional.
  static const double height = 64;

  const HorstKevin({super.key});

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: 'Horst-Kevin — band-heckling dragon',
      child: Image.asset(
        'assets/img/hk-removebg.png',
        height: height,
        fit: BoxFit.contain,
        // PaintingBinding's default image cache keeps the PNG in memory after
        // first decode. `filterQuality: medium` (not high) is sufficient for
        // this small fixed-size asset — high wastes CPU on every layout.
        filterQuality: FilterQuality.medium,
      ),
    );
  }
}

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
    final online = slot?.isOnline ?? false;
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

    final (pillLabel, pillColor) = bandMismatch
        ? ('BAND MISMATCH · $ctrlBand ≠ $radioBand', AppTheme.red)
        : online
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
          // `Row` (not `Wrap`) so RETRACT stays on a single line and the
          // dragon has a fixed slot to the right of it. `crossAxisAlignment:
          // end` aligns the dragon's bottom edge with the RETRACT button's
          // bottom edge — the bottom-of-module visual the user asked for.
          // The `Expanded` spacer absorbs the gap; the dragon sits flush
          // against the right padding edge.
          Row(
            crossAxisAlignment: CrossAxisAlignment.end,
            children: [
              _DirectionButton(
                label: 'FORWARD',
                active: direction == 'forward',
                // Elements moving: lock taps so rapid presses can't queue
                // competing direction cmds against mid-travel motors —
                // the same lockout ultrabridge's own web UI applies.
                onPressed: (online && !moving) ? () => send(antCtrlDirectionPayload('forward')) : null,
              ),
              const SizedBox(width: 4),
              _DirectionButton(
                label: '180°',
                active: direction == 'reverse',
                onPressed: (online && !moving && !on6m) ? () => send(antCtrlDirectionPayload('reverse')) : null,
              ),
              const SizedBox(width: 4),
              _DirectionButton(
                label: 'BI-DIR',
                active: direction == 'bidirectional',
                onPressed: (online && !moving && !on6m) ? () => send(antCtrlDirectionPayload('bidirectional')) : null,
              ),
              const SizedBox(width: 4),
              ElevatedButton(
                // RETRACT stays pressable while moving — it is the emergency
                // action for an unexpected or stuck direction state, and
                // ultrabridge (web UI and handlers alike) keeps it available
                // during travel deliberately.
                onPressed: online ? () => send(antCtrlRetractPayload()) : null,
                style: AppTheme.actionButton(danger: true),
                child: const Text('RETRACT'),
              ),
              const SizedBox(width: 8),
              const Expanded(child: SizedBox.shrink()),
              const HorstKevin(),
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
