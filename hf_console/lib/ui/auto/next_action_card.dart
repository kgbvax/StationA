/// The proposal parts of the DX card of the 75AUTO page: the text of the
/// pending proposal, else of the current action, else why the rules
/// propose nothing (a latched stop outranks all three), the confirm
/// countdown, and Approve and Reject. There is no Resume on the page.
///
/// Approve and Reject show only with a pending proposal. A tap sends the
/// pending id and locks both until the result with the same `cmd_id`
/// arrives, or for 5 s.
library;

import 'package:clock/clock.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_commands.dart';
import 'auto_live.dart';
import 'auto_words.dart';

/// What the card shows, as text.
class NextActionText {
  final String title;
  final String what;
  final Color whatColor;
  final String why;

  const NextActionText(this.title, this.what, this.whatColor, this.why);
}

/// The action as the card writes it: the short capital verb, then the call
/// and the band, for example "QSY 17m" or "CALL VK9XQ · 20m". With [entity]
/// (the DXCC name of the call) it goes after the call:
/// "CALL VK9XQ · Christmas Island · 20m".
String _what(String? action, String? call, String? band, {String? entity}) {
  final target = [
    if (call != null && call.isNotEmpty) call,
    if (entity != null && entity.isNotEmpty) entity,
    if (band != null && band.isNotEmpty) band,
  ].join(' · ');
  return [autoVerb(action), target].where((s) => s.isNotEmpty).join(' ');
}

/// Whether the `current` line of 75auto is still under way. 75auto sends
/// its newest action line as `current` and keeps it after the action ended,
/// for example after the QSO of a call was logged. The card shows it only
/// while it is under way:
/// - a result of `sent`: the action waits for its effect;
/// - a call while its call is still the DX call;
/// - a CQ while there is no DX call.
/// Else the card shows why the rules propose nothing. The action log keeps
/// the finished line.
bool _live(AutoOpView view, AutoOpLogLine c) {
  if (c.result == 'sent') return true;
  final dx = view.station?.dxCall ?? '';
  return switch (c.action) {
    'call-station' => (c.call ?? '').isNotEmpty && c.call == dx,
    'send-cq' => dx.isEmpty,
    _ => false,
  };
}

/// The DXCC name of [call]: 75auto sends it only for the DX call.
String? _entityOf(AutoOpView view, String? call) {
  final st = view.station;
  if (call == null || call.isEmpty || st?.dxCall != call) return null;
  return st?.dxEntity;
}

/// The text of the card for [view].
NextActionText nextActionText(AutoOpView? view) {
  if (view == null) return NextActionText('Next action', 'no data', AppTheme.txtFaint, '');
  if (view.latched == true) {
    final reason = view.stopReason;
    return NextActionText('Stopped', 'LATCHED · resume at the station', AppTheme.red,
        reason == null || reason.isEmpty ? '' : reason);
  }
  final hint = view.hint ?? '';
  final p = view.pending;
  if (p != null) {
    final what = _what(p.action, p.call, p.band, entity: _entityOf(view, p.call));
    final why = [
      if (p.reason != null && p.reason!.isNotEmpty) p.reason!,
      if (p.rule != null && p.rule!.isNotEmpty) 'rule ${p.rule}',
    ].join(' · ');
    return NextActionText('Next action', what, AppTheme.accent, why);
  }
  final c = view.current;
  if (c != null && _live(view, c)) {
    final what = _what(c.action, c.call, c.band, entity: _entityOf(view, c.call));
    final why = [
      ?autoOutcome(c.result),
      if (c.gateReason != null && c.gateReason!.isNotEmpty) c.gateReason!,
      if (c.reason != null && c.reason!.isNotEmpty) c.reason!,
      if (hint.isNotEmpty) hint,
    ].join(' · ');
    return NextActionText('Current action', what, AppTheme.txt, why);
  }
  final idle = view.idle;
  final blocks = idle?.blocks ?? const <AutoOpIdleBlock>[];
  final first = blocks.isEmpty ? null : blocks.first;
  final more = (blocks.length - 1) + (idle?.moreBlocks ?? 0);
  final why = [
    if (first != null) '${first.call ?? '?'}: ${first.why ?? '?'}${more > 0 ? ' (+$more more)' : ''}',
    if (hint.isNotEmpty) hint,
  ].join(' · ');
  return NextActionText('Next action', 'no proposal', AppTheme.txtMute, why);
}

/// `CONFIRM n s` from `deadline_ms` and the moving time of the view.
class AutoCountdown extends StatelessWidget {
  final AutoOpPending pending;

  const AutoCountdown({super.key, required this.pending});

  @override
  Widget build(BuildContext context) {
    return AutoTicker(
      builder: (context, live) {
        final String text;
        if (pending.expired == true) {
          text = 'EXPIRED';
        } else if (pending.deadlineMs == null || live.nowMs == null) {
          text = 'CONFIRM ?';
        } else {
          final left = pending.deadlineMs! - live.nowMs!;
          text = left <= 0 ? 'CONFIRM 0 s' : 'CONFIRM ${(left + 999) ~/ 1000} s';
        }
        // Amber in the last 5 s. Faint while the data is not fresh: then
        // the countdown does not move.
        final soon = pending.expired == true ||
            (pending.deadlineMs != null && live.nowMs != null && pending.deadlineMs! - live.nowMs! < 5000);
        final color = !live.live
            ? AppTheme.txtFaint
            : soon
                ? AppTheme.amber
                : AppTheme.txtMute;
        return Text(text,
            key: const ValueKey('auto-confirm-countdown'),
            maxLines: 1,
            style: AppTheme.mono(11, color: color, weight: FontWeight.w700));
      },
    );
  }
}

/// Approve and Reject of the pending proposal.
class ProposalButtons extends StatelessWidget {
  final AutoOpPending pending;

  const ProposalButtons({super.key, required this.pending});

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    return SizedBox(
      height: 48,
      child: AutoTicker(builder: (context, live) {
        final now = clock.now();
        final gate = autoGateOf(context.read<BusStore>(), live, keyStored: cmd.keyStored);
        final id = pending.id;
        final on = gate.operate == null && id != null && !cmd.proposalLocked(now);
        final problem = cmd.problemControl(gate, now);
        return Row(children: [
          Expanded(
            child: _button(AutoControl.approve,
                primary: true, problem: problem == AutoControl.approve, onTap: on ? () => cmd.send(AutoControl.approve, id) : null),
          ),
          const SizedBox(width: 6),
          Expanded(
            child: _button(AutoControl.reject,
                problem: problem == AutoControl.reject, onTap: on ? () => cmd.send(AutoControl.reject, id) : null),
          ),
        ]);
      }),
    );
  }

  Widget _button(AutoControl c, {bool primary = false, bool problem = false, VoidCallback? onTap}) {
    final Color line = problem
        ? AppTheme.amber
        : primary
            ? AppTheme.blend(AppTheme.accent, onTap == null ? 0.5 : 0.9)
            : AppTheme.cardLine;
    // Approve is the filled accent button while it can be tapped.
    final filled = primary && onTap != null;
    return ElevatedButton(
      key: ValueKey('auto-${c.name}'),
      onPressed: onTap,
      style: AppTheme.actionButton().copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
        side: WidgetStatePropertyAll(BorderSide(width: problem ? 1.4 : 1, color: line)),
        backgroundColor: filled ? WidgetStatePropertyAll(AppTheme.accent) : null,
      ),
      child: Text(c.label,
          style: AppTheme.display(15,
              color: onTap == null ? AppTheme.txtFaint : (filled ? AppTheme.activeButtonText : null),
              weight: FontWeight.w600,
              letterSpacing: 0.06)),
    );
  }
}
