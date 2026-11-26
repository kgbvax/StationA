/// The Next action card of the 75AUTO page: the pending proposal with its
/// confirm countdown, else the current action, else why the rules propose
/// nothing. A latched stop outranks all three. Approve and Reject sit at the
/// bottom of the card, disabled in this build.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_live.dart';

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
    final what = [p.action ?? '?', _target(p.call, p.band)].where((s) => s.isNotEmpty).join(' ');
    final why = [
      if (p.reason != null && p.reason!.isNotEmpty) p.reason!,
      if (p.rule != null && p.rule!.isNotEmpty) 'rule ${p.rule}',
    ].join(' · ');
    return NextActionText('Next action', what, AppTheme.accent, why);
  }
  final c = view.current;
  if (c != null) {
    final what = [c.action ?? '?', _target(c.call, c.band)].where((s) => s.isNotEmpty).join(' ');
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
        style: AppTheme.mono(17, color: t.whatColor, weight: FontWeight.w600).copyWith(height: 1.25));
    final buttons = SizedBox(
      height: 48,
      child: Row(children: [
        Expanded(child: _disabled('Approve', primary: true)),
        const SizedBox(width: 6),
        Expanded(child: _disabled('Reject')),
      ]),
    );
    return LayoutBuilder(builder: (context, c) {
      // Under a banner on a landscape phone the card gets less than its
      // full height. Then the title strip goes, and the countdown moves
      // next to the proposal, so Approve and Reject keep 48 dp.
      if (c.maxHeight < compactBelow) {
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
              buttons,
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
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              what,
              Expanded(
                child: LayoutBuilder(builder: (context, c) {
                  // As many lines of the reason as fit. None on a short screen.
                  final lines = (c.maxHeight / 16).floor();
                  if (lines < 1 || t.why.isEmpty) return const SizedBox.shrink();
                  return Text(t.why,
                      key: const ValueKey('auto-next-why'),
                      maxLines: lines,
                      overflow: TextOverflow.ellipsis,
                      style: AppTheme.body(11, color: AppTheme.txtMute).copyWith(height: 1.2));
                }),
              ),
              buttons,
            ],
          ),
        ),
      );
    });
  }

  /// Below this height the card drops its title strip.
  static const compactBelow = 104.0;

  Widget _disabled(String label, {bool primary = false}) => ElevatedButton(
        onPressed: null,
        style: AppTheme.actionButton().copyWith(
          minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
          side: WidgetStatePropertyAll(BorderSide(color: primary ? AppTheme.blend(AppTheme.accent, 0.5) : AppTheme.cardLine)),
        ),
        child: Text(label.toUpperCase(), style: AppTheme.display(15, weight: FontWeight.w600, letterSpacing: 0.06)),
      );
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
        return Text(text,
            key: const ValueKey('auto-confirm-countdown'),
            maxLines: 1,
            style: AppTheme.mono(11, color: live.live ? AppTheme.amber : AppTheme.txtFaint, weight: FontWeight.w700));
      },
    );
  }
}
