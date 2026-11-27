/// The control column of the 75AUTO page, from top to bottom: the Next
/// action card with Approve and Reject, the TARGET card (Follow, Skip) and
/// the MODE card (Observe, Confirm, Auto) side by side, and Stop TX at the
/// bottom. The feedback line sits in the title strip of the MODE card.
///
/// Each tap sends one signed command (`auto_commands.dart`). There are no
/// dialogs: the gate of 75auto and the pending id are the interlocks.
///
/// - Stop TX is in the danger style, filled red while 75auto transmits. It
///   stays on with stale data and never locks. Its second line shows the
///   reason when it is off, else what became of the last stop.
/// - The row and Approve and Reject need fresh data and `operate`. The
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
import 'next_action_card.dart';

/// The height of the buttons of the TARGET and MODE cards, and of Stop TX.
const autoButtonRowHeight = 48.0;
const autoStopHeight = 60.0;

/// The title strip of the TARGET and MODE cards.
const autoRowTitleHeight = 16.0;

/// The height of the TARGET and MODE cards.
const autoRowCardHeight = autoRowTitleHeight + autoButtonRowHeight + 2 * 2 + 2;

class AutoControlColumn extends StatelessWidget {
  /// The fixed height of the Next action card. Null lets it fill the rest
  /// of the column (landscape).
  final double? cardHeight;

  const AutoControlColumn({super.key, this.cardHeight});

  @override
  Widget build(BuildContext context) {
    const card = NextActionCard();
    return Column(
      mainAxisSize: cardHeight == null ? MainAxisSize.max : MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (cardHeight == null) const Expanded(child: card) else SizedBox(height: cardHeight, child: card),
        const SizedBox(height: 4),
        const SizedBox(height: autoRowCardHeight, child: _ButtonRow()),
        const SizedBox(height: 4),
        const SizedBox(height: autoStopHeight, child: _StopTx()),
      ],
    );
  }
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
      final Color label = hot
          ? AppTheme.activeButtonText
          : on
              ? AppTheme.red
              : AppTheme.blend(AppTheme.red, 0.6);
      final style = AppTheme.actionButton(danger: true, dangerActive: hot).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(48, autoStopHeight)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 2)),
      );
      return ElevatedButton(
        key: const ValueKey('auto-stop-tx'),
        // Never locked: each tap sends a new stop.
        onPressed: on ? () => cmd.send(AutoControl.stop, null) : null,
        style: on
            ? style
            : style.copyWith(
                side: WidgetStatePropertyAll(BorderSide(width: 1.4, color: AppTheme.blend(AppTheme.red, 0.6))),
                backgroundColor: WidgetStatePropertyAll(AppTheme.blend(AppTheme.red, 0.06)),
              ),
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
            Text(line?.text ?? '',
                key: const ValueKey('auto-stop-line'),
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: AppTheme.mono(10,
                        color: hot ? AppTheme.activeButtonText : (line?.color ?? AppTheme.txtMute),
                        weight: FontWeight.w600)
                    .copyWith(height: 1.2)),
          ],
        ),
      );
    });
  }
}
