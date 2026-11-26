/// The band activity list of the 75AUTO page, as in WSJT-X: the decodes of
/// the latest periods, newest period first, with a header per period.
///
/// All text from the air shows with a plain [Text]. Nothing is parsed, so
/// `<b>CQ</b>` shows with its angle brackets.
///
/// The list rebuilds only with a new /state document, which 75auto
/// publishes at most 2 times per second. A /beat does not rebuild it.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_live.dart';

/// The fixed height of one row of the lists.
const autoRowHeight = 24.0;

/// One row of a list: a period header, a decode, or an own transmission.
class AutoRow {
  final String? header;
  final AutoOpDecode? decode;
  final AutoOpTxLine? tx;

  /// The row belongs to an older period than the newest in the list.
  final bool old;

  const AutoRow.header(this.header)
      : decode = null,
        tx = null,
        old = false;
  const AutoRow.decode(this.decode, {this.old = false})
      : header = null,
        tx = null;
  const AutoRow.tx(this.tx, {this.old = false})
      : header = null,
        decode = null;
}

/// The rows of the band activity list: the decodes of [view], newest period
/// first, each period under a header with its time and EVEN or ODD.
///
/// The document holds only the decodes of the current band (contract §5.2).
/// The decodes carry no band of their own, so there is nothing more to
/// filter here.
List<AutoRow> bandActivityRows(AutoOpView? view) {
  final decodes = view?.decodes ?? const <AutoOpDecode>[];
  final period = view?.station?.periodMs;
  final byPeriod = <int, List<AutoOpDecode>>{};
  for (final d in decodes) {
    byPeriod.putIfAbsent(d.periodStartMs ?? 0, () => []).add(d);
  }
  final starts = byPeriod.keys.toList()..sort((a, b) => b.compareTo(a));
  final newest = starts.isEmpty ? null : starts.first;
  return [
    for (final p in starts) ...[
      AutoRow.header(p == 0 ? 'period ?' : [utcHms(p), ?parityOf(p, period)].join(' · ')),
      for (final d in byPeriod[p]!) AutoRow.decode(d, old: p != newest),
    ],
  ];
}

class DecodeList extends StatelessWidget {
  const DecodeList({super.key});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final rows = bandActivityRows(view);
    return AutoCard(
      title: 'Band activity',
      trailing: Text('CQ green · NEW needed · TGT target',
          maxLines: 1, overflow: TextOverflow.ellipsis, style: AppTheme.display(10, color: AppTheme.txtFaint)),
      child: rows.isEmpty
          ? Center(child: Text(view == null ? 'no data' : 'no decodes', style: AppTheme.mono(12, color: AppTheme.txtFaint)))
          : ListView.builder(
              key: const ValueKey('auto-decode-list'),
              itemExtent: autoRowHeight,
              itemCount: rows.length,
              itemBuilder: (context, i) => AutoRowView(row: rows[i], dxCall: view?.station?.dxCall),
            ),
    );
  }
}

/// One row of the band activity list or the RX frequency list.
class AutoRowView extends StatelessWidget {
  final AutoRow row;
  final String? dxCall;

  /// Only the TO ME tag on a decode to me (the RX list, where the text
  /// matters more than the other tags).
  final bool toMeTagOnly;

  const AutoRowView({super.key, required this.row, required this.dxCall, this.toMeTagOnly = false});

  @override
  Widget build(BuildContext context) {
    if (row.header != null) {
      return Container(
        color: AppTheme.pane,
        padding: const EdgeInsets.symmetric(horizontal: 8),
        alignment: Alignment.centerLeft,
        child: Text(row.header!, style: AppTheme.mono(10, color: AppTheme.txtFaint, letterSpacing: 0.08)),
      );
    }
    final d = row.decode;
    final tx = row.tx;
    final String snr;
    final int? df;
    final String text;
    var tags = <Widget>[];
    Color color;
    Color? fill;
    var weight = FontWeight.w400;
    if (tx != null) {
      snr = 'TX';
      df = tx.df;
      text = tx.text ?? '?';
      color = AppTheme.orange;
    } else {
      final v = d!.snr;
      snr = v == null ? '?' : (v >= 0 ? '+${v.toString().padLeft(2, '0')}' : '-${(-v).toString().padLeft(2, '0')}');
      df = d.df;
      text = d.text ?? '?';
      final tgt = dxCall != null && dxCall!.isNotEmpty && d.call == dxCall;
      color = row.old ? AppTheme.txtFaint : (d.cq ? AppTheme.green : AppTheme.txt);
      if (tgt) {
        color = AppTheme.accent;
        fill = AppTheme.accentDim;
        weight = FontWeight.w600;
        tags.add(AutoTag('TGT', AppTheme.accent));
      }
      if (d.needed) tags.add(AutoTag('NEW', AppTheme.amber));
      if (d.toMe) {
        color = AppTheme.red;
        fill = AppTheme.blend(AppTheme.red, 0.12);
        weight = FontWeight.w600;
        if (toMeTagOnly) tags = [];
        tags.add(AutoTag('TO ME', AppTheme.red));
      }
    }
    return Container(
      decoration: BoxDecoration(
        color: fill,
        border: Border(bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 6),
      child: Row(
        children: [
          SizedBox(
            width: 28,
            child: Text(snr, style: AppTheme.mono(11, color: tx != null ? AppTheme.orange : AppTheme.txtFaint)),
          ),
          SizedBox(
            width: 38,
            child: Text(df == null ? '?' : '$df',
                textAlign: TextAlign.right, style: AppTheme.mono(11, color: AppTheme.txtMute)),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: Text(text,
                maxLines: 1,
                softWrap: false,
                overflow: TextOverflow.ellipsis,
                style: AppTheme.mono(13, color: color, weight: weight)),
          ),
          ...tags,
        ],
      ),
    );
  }
}
