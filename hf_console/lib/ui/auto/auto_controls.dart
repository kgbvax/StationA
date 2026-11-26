/// The control column of the 75AUTO page, from top to bottom: the Next
/// action card with Approve and Reject, one row with Follow, Skip, Observe,
/// Confirm, and Auto, and Stop TX at the bottom.
///
/// This build is read-only. The controls sit in their final places and
/// sizes, so the height budget of the page holds, but they are disabled and
/// send nothing. One line in the Stop TX block gives the reason. The
/// buttons still show the state: the active mode and an active Follow have
/// an accent border.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'next_action_card.dart';

/// The reason that every control of this build is off.
const autoControlsOffReason = 'Commands come in a later build';

/// The height of the button row and of Stop TX.
const autoButtonRowHeight = 48.0;
const autoStopHeight = 60.0;

class AutoControlColumn extends StatelessWidget {
  /// The fixed height of the Next action card. Null lets it fill the rest
  /// of the column (landscape).
  final double? cardHeight;

  const AutoControlColumn({super.key, this.cardHeight});

  @override
  Widget build(BuildContext context) {
    final station = context.select<BusStore, AutoOpStation?>((s) => s.autoOp?.station);
    final mode = context.select<BusStore, String?>((s) => s.autoOp?.session?.mode);
    final follow = station?.targetChoice == 'follow';
    final skip = station?.targetChoice == 'skip';
    const card = NextActionCard();
    return Column(
      mainAxisSize: cardHeight == null ? MainAxisSize.max : MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (cardHeight == null) const Expanded(child: card) else SizedBox(height: cardHeight, child: card),
        const SizedBox(height: 4),
        SizedBox(
          height: autoButtonRowHeight,
          child: Row(children: [
            Expanded(child: _OffButton('Follow', on: follow, dot: follow)),
            const SizedBox(width: 4),
            Expanded(child: _OffButton('Skip', on: skip)),
            const SizedBox(width: 4),
            Expanded(child: _OffButton('Observe', on: mode == 'observe')),
            const SizedBox(width: 4),
            Expanded(child: _OffButton('Confirm', on: mode == 'confirm')),
            const SizedBox(width: 4),
            Expanded(child: _OffButton('Auto', on: mode == 'auto')),
          ]),
        ),
        const SizedBox(height: 4),
        const SizedBox(height: autoStopHeight, child: _StopTx()),
      ],
    );
  }
}

class _OffButton extends StatelessWidget {
  final String label;
  final bool on;

  /// A dot ahead of the label: an active Follow.
  final bool dot;

  const _OffButton(this.label, {required this.on, this.dot = false});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: null,
      style: AppTheme.actionButton().copyWith(
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 2)),
        minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
        side: WidgetStatePropertyAll(BorderSide(width: on ? 1.4 : 1, color: on ? AppTheme.accent : AppTheme.cardLine)),
        backgroundColor: WidgetStatePropertyAll(on ? AppTheme.accentDim : AppTheme.pane),
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
            label.toUpperCase(),
            maxLines: 1,
            style: AppTheme.display(13,
                color: on ? AppTheme.blend(AppTheme.accent, 0.8) : null, weight: FontWeight.w600, letterSpacing: 0.04),
          ),
        ]),
      ),
    );
  }
}

/// Stop TX, always in view at the bottom of the column. Off in this build,
/// with the reason in its second line.
class _StopTx extends StatelessWidget {
  const _StopTx();

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      key: const ValueKey('auto-stop-tx'),
      onPressed: null,
      style: AppTheme.actionButton(danger: true).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(48, autoStopHeight)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 2)),
        side: WidgetStatePropertyAll(BorderSide(width: 1.4, color: AppTheme.blend(AppTheme.red, 0.6))),
        backgroundColor: WidgetStatePropertyAll(AppTheme.blend(AppTheme.red, 0.06)),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(mainAxisSize: MainAxisSize.min, children: [
            Container(width: 13, height: 13, color: AppTheme.blend(AppTheme.red, 0.6)),
            const SizedBox(width: 8),
            Text('STOP TX',
              style: AppTheme.display(19,
                      color: AppTheme.blend(AppTheme.red, 0.6), weight: FontWeight.w700, letterSpacing: 0.08)
                  .copyWith(height: 1.15)),
          ]),
          Text(autoControlsOffReason,
              key: const ValueKey('auto-controls-off-reason'),
              maxLines: 1,
              style: AppTheme.mono(10, color: AppTheme.txtMute).copyWith(height: 1.2)),
        ],
      ),
    );
  }
}
