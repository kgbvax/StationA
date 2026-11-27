/// The action log of the 75AUTO page: a band change in progress, then the
/// lines of the log, newest first. The two newest lines are bright, the
/// older ones faint, a refusal amber. Plain text only.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_live.dart';
import 'auto_words.dart';

/// One entry of the strip.
class AutoLogEntry {
  final String text;
  final Color color;

  const AutoLogEntry(this.text, this.color);
}

/// The entries of the strip for [view], newest first.
List<AutoLogEntry> actionLogEntries(AutoOpView? view) {
  final out = <AutoLogEntry>[];
  final bc = view?.station?.bandChange;
  if (bc != null && (bc.target ?? '').isNotEmpty) {
    final outcome = (bc.result ?? '').isEmpty ? null : bc.result!.replaceAll('-', ' ').toUpperCase();
    out.add(AutoLogEntry('QSY ${view?.station?.band ?? '?'}→${bc.target}${outcome == null ? '' : ' $outcome'}', AppTheme.amber));
  }
  final log = view?.log ?? const <AutoOpLogLine>[];
  var n = 0;
  for (final l in log.reversed) {
    // "14:27 CALL VK9XQ 20m OK": the short verb, the call, the band, and
    // the outcome word.
    final outcome = autoOutcome(l.result);
    final parts = <String>[
      if (l.timeMs != null) utcHm(l.timeMs!),
      autoVerb(l.action),
      if ((l.call ?? '').isNotEmpty) l.call!,
      if ((l.band ?? '').isNotEmpty) l.band!,
      ?outcome,
      if ((l.gateReason ?? '').isNotEmpty) '(${l.gateReason})',
    ];
    final refused = l.result == 'refused';
    final color = refused ? AppTheme.amber : (n < 2 ? AppTheme.txt : AppTheme.txtFaint);
    out.add(AutoLogEntry(parts.join(' '), color));
    n++;
  }
  return out;
}

class ActionLogStrip extends StatelessWidget {
  /// A column of lines (portrait) instead of one row.
  final bool vertical;

  const ActionLogStrip({super.key, this.vertical = false});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final entries = actionLogEntries(view);
    Widget line(AutoLogEntry e) =>
        Text(e.text, maxLines: 1, softWrap: false, overflow: TextOverflow.ellipsis, style: AppTheme.mono(12, color: e.color));
    if (vertical) {
      return Container(
        decoration: AppTheme.cardDecoration(),
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: entries.isEmpty
              ? [Text('no actions yet', style: AppTheme.mono(12, color: AppTheme.txtFaint))]
              : [for (final e in entries) line(e)],
        ),
      );
    }
    return Container(
      decoration: AppTheme.cardDecoration(),
      padding: const EdgeInsets.symmetric(horizontal: 10),
      child: entries.isEmpty
          ? Align(alignment: Alignment.centerLeft, child: Text('no actions yet', style: AppTheme.mono(12, color: AppTheme.txtFaint)))
          : ListView.separated(
              key: const ValueKey('auto-action-log'),
              scrollDirection: Axis.horizontal,
              itemCount: entries.length,
              separatorBuilder: (_, _) => const SizedBox(width: 18),
              itemBuilder: (context, i) => Center(child: line(entries[i])),
            ),
    );
  }
}
