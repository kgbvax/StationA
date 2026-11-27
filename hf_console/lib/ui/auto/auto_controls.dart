/// The controls of the 75AUTO page: the TARGET card (Follow, Skip) and the
/// MODE card (Observe, Confirm, Auto) side by side, then Arm CQ and Stop TX.
/// In landscape the TARGET and MODE cards sit under the band activity list,
/// and Arm CQ and Stop TX under the RX frequency list. The feedback line sits in the
/// title strip of the MODE card. Approve and Reject are in the DX card.
///
/// Arm CQ sits next to Stop TX, as in the window of 75auto: Arm CQ gives the
/// rules the permission to call CQ for 30 minutes, Stop takes it away. It
/// is on in auto mode under the same gate as the other commands. While the
/// arm lasts it shows the time that is left, and a tap arms for 30 minutes
/// from now again.
///
/// Each tap sends one signed command (`auto_commands.dart`). There are no
/// dialogs: the gate of 75auto and the pending id are the interlocks.
///
/// - Stop TX has a red frame while 75auto transmits, and the plain frame
///   otherwise. When it is off, it has the disabled colors. It stays on
///   with stale data and never locks. Its second line shows the
///   reason when it is off, else what became of the last stop.
/// - The cards and Approve and Reject need fresh data and `operate`. The
///   feedback line shows the reason when they are off, else the result of
///   the last command. A refused command also gives its button an amber
///   border.
library;

import 'package:clock/clock.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_commands.dart';
import 'auto_live.dart';

/// The height of the buttons of the TARGET and MODE cards, and of Stop TX.
const autoButtonRowHeight = 48.0;
const autoStopHeight = 60.0;

/// The title strip of the TARGET and MODE cards.
const autoRowTitleHeight = 16.0;

/// The height of the TARGET and MODE cards.
const autoRowCardHeight = autoRowTitleHeight + autoButtonRowHeight + 2 * 2 + 2;

/// The height of the control block: the TARGET and MODE cards, and Arm CQ
/// and Stop TX.
const autoControlsHeight = autoRowCardHeight + 4 + autoStopHeight;

/// The control block of the 75AUTO page: the TARGET and MODE cards, then
/// Arm CQ and Stop TX. Without [targetMode] it holds only Arm CQ and Stop
/// TX, and [AutoTargetMode] shows the cards elsewhere.
class AutoControls extends StatelessWidget {
  final bool targetMode;

  const AutoControls({super.key, this.targetMode = true});

  @override
  Widget build(BuildContext context) {
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (targetMode) ...const [AutoTargetMode(), SizedBox(height: 4)],
        const SizedBox(
          height: autoStopHeight,
          child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            // Arm CQ and Stop TX share the row in equal halves.
            Expanded(child: _ArmCq()),
            SizedBox(width: 4),
            Expanded(child: _StopTx()),
          ]),
        ),
      ],
    );
  }
}

/// The TARGET card (Follow, Skip) and the MODE card (Observe, Confirm,
/// Auto), side by side, at their fixed height.
class AutoTargetMode extends StatelessWidget {
  const AutoTargetMode({super.key});

  @override
  Widget build(BuildContext context) => const SizedBox(height: autoRowCardHeight, child: _ButtonRow());
}

/// The TARGET card (Follow, Skip) and the MODE card (Observe, Confirm,
/// Auto), side by side.
class _ButtonRow extends StatelessWidget {
  const _ButtonRow();

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    return AutoTicker(builder: (context, live) {
      final store = context.read<BusStore>();
      final gate = autoGateOf(store, live, keyStored: cmd.keyStored);
      final on = gate.operate == null;
      final problem = cmd.problemControl(gate, clock.now());
      final station = store.autoOp?.station;
      final mode = store.autoOp?.session?.mode;
      final dx = station?.dxCall;
      final hasDx = dx != null && dx.isNotEmpty;
      final follow = station?.targetChoice == 'follow';
      final skip = station?.targetChoice == 'skip';

      Widget button(AutoControl c, {required bool active, required VoidCallback? onTap, bool dot = false}) =>
          _RowButton(c, active: active, onTap: onTap, dot: dot, problem: problem == c);

      VoidCallback? modeTap(AutoControl c, String value) {
        if (!on) return null;
        // A tap on the active mode sends nothing.
        if (mode == value) return () {};
        return () => cmd.send(c, value);
      }

      Widget card(String title, List<Widget> buttons, {Widget? trailing}) => AutoCard(
            title: title,
            titleHeight: autoRowTitleHeight,
            trailing: trailing,
            child: Padding(
              padding: const EdgeInsets.all(2),
              child: Row(children: [
                for (var i = 0; i < buttons.length; i++) ...[
                  if (i > 0) const SizedBox(width: 3),
                  Expanded(child: buttons[i]),
                ],
              ]),
            ),
          );

      return Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Expanded(
          flex: 2,
          child: card('Target', [
            button(AutoControl.follow,
                active: follow,
                dot: follow,
                // A second tap on an active Follow clears it.
                onTap: on && hasDx
                    ? () => cmd.send(AutoControl.follow, {'call': dx, 'choice': follow ? 'clear' : 'follow'})
                    : null),
            button(AutoControl.skip,
                active: skip,
                onTap: on && hasDx ? () => cmd.send(AutoControl.skip, {'call': dx, 'choice': 'skip'}) : null),
          ]),
        ),
        const SizedBox(width: 4),
        Expanded(
          flex: 3,
          child: card('Mode', [
            button(AutoControl.observe, active: mode == 'observe', onTap: modeTap(AutoControl.observe, 'observe')),
            button(AutoControl.confirm, active: mode == 'confirm', onTap: modeTap(AutoControl.confirm, 'confirm')),
            button(AutoControl.auto, active: mode == 'auto', onTap: modeTap(AutoControl.auto, 'auto')),
          ], trailing: const _OperateLine()),
        ),
      ]);
    });
  }
}

class _RowButton extends StatelessWidget {
  final AutoControl control;
  final bool active;
  final VoidCallback? onTap;

  /// A dot ahead of the label: an active Follow.
  final bool dot;

  /// The last command of this button was refused or got no answer.
  final bool problem;

  const _RowButton(this.control, {required this.active, required this.onTap, this.dot = false, this.problem = false});

  @override
  Widget build(BuildContext context) {
    final enabled = onTap != null;
    final Color text = active
        ? AppTheme.blend(AppTheme.accent, 0.8)
        : enabled
            ? AppTheme.txt
            : AppTheme.txtFaint;
    final Color line = problem
        ? AppTheme.amber
        : active
            ? AppTheme.accent
            : AppTheme.cardLine;
    return ElevatedButton(
      key: ValueKey('auto-${control.name}'),
      onPressed: onTap,
      style: AppTheme.actionButton().copyWith(
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 2)),
        minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
        side: WidgetStatePropertyAll(BorderSide(width: active || problem ? 1.4 : 1, color: line)),
        backgroundColor: WidgetStatePropertyAll(active ? AppTheme.accentDim : AppTheme.pane),
      ),
      child: FittedBox(
        fit: BoxFit.scaleDown,
        child: Row(mainAxisSize: MainAxisSize.min, children: [
          if (dot) ...[
            Container(
              width: 7,
              height: 7,
              decoration: BoxDecoration(color: AppTheme.blend(AppTheme.accent, 0.8), shape: BoxShape.circle),
            ),
            const SizedBox(width: 4),
          ],
          Text(
            control.label,
            maxLines: 1,
            style: AppTheme.display(13, color: text, weight: FontWeight.w600, letterSpacing: 0.04),
          ),
        ]),
      ),
    );
  }
}

/// The one line of the TARGET and MODE cards and of Approve and Reject: why
/// they are off, or the result of the last command. It sits in the title
/// strip of the MODE card.
class _OperateLine extends StatelessWidget {
  const _OperateLine();

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    return AutoTicker(builder: (context, live) {
      final gate = autoGateOf(context.read<BusStore>(), live, keyStored: cmd.keyStored);
      final line = cmd.operateLine(gate, clock.now());
      // A long reason scales down a little rather than lose its end.
      return FittedBox(
        fit: BoxFit.scaleDown,
        alignment: Alignment.centerRight,
        child: Text(line?.text ?? '',
            key: const ValueKey('auto-operate-line'),
            maxLines: 1,
            style: AppTheme.mono(10, color: line?.color ?? AppTheme.txtMute, weight: FontWeight.w600)
                .copyWith(height: 1.3)),
      );
    });
  }
}

/// Arm CQ. Off: ARM CQ. Armed by the operator: CQ and the time left.
/// Confirm mode: CQ CONFIRM, off (each CQ needs an approval there).
class _ArmCq extends StatelessWidget {
  const _ArmCq();

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    return AutoTicker(builder: (context, live) {
      final store = context.read<BusStore>();
      final gate = autoGateOf(store, live, keyStored: cmd.keyStored);
      final view = store.autoOp;
      final auto = view?.session?.mode == 'auto';
      final arm = view?.arm;
      final now = live.nowMs;
      final left = arm?.source == 'operator' && arm?.untilMs != null && now != null ? arm!.untilMs! - now : 0;
      final armed = left > 0;
      final confirm = arm?.source == 'confirm';
      final on = gate.operate == null && auto;
      final problem = cmd.problemControl(gate, clock.now()) == AutoControl.armCq;
      final String top;
      final String? sub;
      if (confirm) {
        top = 'CQ';
        sub = 'CONFIRM';
      } else if (armed) {
        final s = (left + 999) ~/ 1000;
        top = 'CQ';
        sub = '${s ~/ 60}:${(s % 60).toString().padLeft(2, '0')}';
      } else {
        top = 'ARM CQ';
        sub = auto ? null : 'auto only';
      }
      final Color text = armed ? AppTheme.blend(AppTheme.accent, 0.9) : (on ? AppTheme.txt : AppTheme.txtFaint);
      return ElevatedButton(
        key: const ValueKey('auto-arm-cq'),
        onPressed: on ? () => cmd.send(AutoControl.armCq, null) : null,
        style: AppTheme.actionButton().copyWith(
          padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 4, vertical: 2)),
          minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
          side: WidgetStatePropertyAll(BorderSide(
              width: armed || problem ? 1.4 : 1,
              color: problem ? AppTheme.amber : (armed ? AppTheme.accent : AppTheme.cardLine))),
          backgroundColor: WidgetStatePropertyAll(armed ? AppTheme.accentDim : AppTheme.pane),
        ),
        child: FittedBox(
          fit: BoxFit.scaleDown,
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Text(top,
                key: const ValueKey('auto-arm-cq-label'),
                style: AppTheme.display(15, color: text, weight: FontWeight.w700, letterSpacing: 0.06)),
            if (sub != null)
              Text(sub,
                  key: const ValueKey('auto-arm-cq-sub'),
                  style: AppTheme.mono(11, color: armed ? text : AppTheme.txtMute, weight: FontWeight.w600)),
          ]),
        ),
      );
    });
  }
}

/// Stop TX, always in view at the bottom of the column.
class _StopTx extends StatelessWidget {
  const _StopTx();

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    final transmitting = context.select<BusStore, bool>((s) => s.autoOp?.station?.transmitting == true);
    return AutoTicker(builder: (context, live) {
      final gate = autoGateOf(context.read<BusStore>(), live, keyStored: cmd.keyStored);
      final on = gate.stop == null;
      final line = cmd.stopLine(gate, clock.now());
      final hot = on && transmitting;
      // While 75auto transmits, only the frame turns red: a filled red
      // button is too loud for the car screen. Idle, the button is a plain
      // button; off, it has the disabled colors of every button.
      final Color label = on ? AppTheme.txt : AppTheme.txtFaint;
      final style = AppTheme.actionButton().copyWith(
        side: WidgetStatePropertyAll(BorderSide(width: hot ? 2 : 1.4, color: hot ? AppTheme.red : AppTheme.cardLineHi)),
        minimumSize: const WidgetStatePropertyAll(Size(48, autoStopHeight)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 2)),
      );
      return ElevatedButton(
        key: const ValueKey('auto-stop-tx'),
        // Never locked: each tap sends a new stop.
        onPressed: on ? () => cmd.send(AutoControl.stop, null) : null,
        style: style,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Row(mainAxisSize: MainAxisSize.min, children: [
              Container(width: 13, height: 13, color: label),
              const SizedBox(width: 8),
              Text('STOP TX',
                  style: AppTheme.display(19, color: label, weight: FontWeight.w700, letterSpacing: 0.08)
                      .copyWith(height: 1.15)),
            ]),
            // Only with text, so a plain STOP TX sits in the middle.
            if ((line?.text ?? '').isNotEmpty)
              Text(line!.text,
                  key: const ValueKey('auto-stop-line'),
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: AppTheme.mono(10,
                          color: line.color,
                          weight: FontWeight.w600)
                      .copyWith(height: 1.2)),
          ],
        ),
      );
    });
  }
}
