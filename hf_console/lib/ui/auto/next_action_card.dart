/// The Next action card of the 75AUTO page: the pending proposal with its
/// confirm countdown, else the current action, else why the rules propose
/// nothing. A latched stop outranks all three: then the card says
/// "LATCHED · resume at the station" with the stop reason, and it has no
/// buttons. There is no Resume on the page.
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
import 'auto_card.dart';
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

String _target(String? call, String? band) => [
      if (call != null && call.isNotEmpty) call,
      if (band != null && band.isNotEmpty) band,
    ].join(' · ');

/// The action as the card writes it: the short capital verb, then the call
/// and the band, for example "QSY 17m" or "CALL VK9XQ · 20m".
String _what(String? action, String? call, String? band) =>
    [autoVerb(action), _target(call, band)].where((s) => s.isNotEmpty).join(' ');

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
    final what = _what(p.action, p.call, p.band);
    final why = [
      if (p.reason != null && p.reason!.isNotEmpty) p.reason!,
      if (p.rule != null && p.rule!.isNotEmpty) 'rule ${p.rule}',
    ].join(' · ');
    return NextActionText('Next action', what, AppTheme.accent, why);
  }
  final c = view.current;
  if (c != null) {
    final what = _what(c.action, c.call, c.band);
    final why = [
      if (c.result != null && c.result!.isNotEmpty) c.result!,
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

class NextActionCard extends StatelessWidget {
  const NextActionCard({super.key});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final t = nextActionText(view);
    final pending = view?.latched == true ? null : view?.pending;
    final what = Text(t.what,
        key: const ValueKey('auto-next-what'),
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: AppTheme.display(19, color: t.whatColor, weight: FontWeight.w700, letterSpacing: 0.04).copyWith(height: 1.2));
    final Widget? buttons = pending == null ? null : _ProposalButtons(pending: pending);
    return LayoutBuilder(builder: (context, c) {
      // Under a banner on a landscape phone the card gets less than its
      // full height. Then the title strip goes, and the countdown moves
      // next to the proposal, so Approve and Reject keep 48 dp.
      if (c.maxHeight < (buttons == null ? compactBelowNoButtons : compactBelow)) {
        return Container(
          key: const ValueKey('auto-next-action'),
          decoration: AppTheme.cardDecoration(),
          padding: const EdgeInsets.fromLTRB(8, 2, 8, 4),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Expanded(
                child: Row(children: [
                  Expanded(child: what),
                  if (pending != null) _Countdown(pending: pending),
                ]),
              ),
              ?buttons,
            ],
          ),
        );
      }
      return AutoCard(
        key: const ValueKey('auto-next-action'),
        title: t.title,
        trailing: pending == null ? null : _Countdown(pending: pending),
        child: Padding(
          padding: const EdgeInsets.fromLTRB(8, 4, 8, 6),
          child: LayoutBuilder(builder: (context, c) {
            // The proposal, its reason, then Approve and Reject right under
            // the reason. The reason gets the lines that fit, at most 3.
            const whatHeight = 24.0, lineHeight = 14.0, buttonsHeight = 48.0 + 6;
            final room = c.maxHeight - whatHeight - (buttons == null ? 0 : buttonsHeight);
            final lines = t.why.isEmpty ? 0 : (room / lineHeight).floor().clamp(0, 3);
            return Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                what,
                if (lines > 0)
                  Text(t.why,
                      key: const ValueKey('auto-next-why'),
                      maxLines: lines,
                      overflow: TextOverflow.ellipsis,
                      style: AppTheme.body(11, color: AppTheme.txtMute).copyWith(height: 1.2)),
                if (buttons != null) ...[const SizedBox(height: 6), buttons],
              ],
            );
          }),
        ),
      );
    });
  }

  /// Below this height the card drops its title strip: the full card needs
  /// the title, the proposal, and Approve and Reject.
  static const compactBelow = 112.0;

  /// The same without Approve and Reject: the title, the text, one line of
  /// the reason.
  static const compactBelowNoButtons = 72.0;
}

/// `CONFIRM n s` from `deadline_ms` and the moving time of the view.
class _Countdown extends StatelessWidget {
  final AutoOpPending pending;

  const _Countdown({required this.pending});

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
class _ProposalButtons extends StatelessWidget {
  final AutoOpPending pending;

  const _ProposalButtons({required this.pending});

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
