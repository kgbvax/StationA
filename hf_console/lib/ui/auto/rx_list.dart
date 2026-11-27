/// The RX frequency list of the 75AUTO page: the own transmissions, the
/// decodes within 50 Hz of the RX offset, and every decode to me. Oldest
/// first, as in WSJT-X. The list starts at the top. When it is longer than
/// the card, the newest line stays in view at the bottom.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'decode_list.dart';

/// The width of the RX window on each side of `rx_df`.
const rxWindowHz = 50;

/// The rows of the RX frequency list, oldest first. Own lines of another
/// band than `station.band` stay out.
List<AutoRow> rxRows(AutoOpView? view) {
  final st = view?.station;
  final rx = st?.rxDf;
  final band = st?.band;
  final entries = <(int, int, AutoRow)>[];
  var i = 0;
  for (final t in view?.txLines ?? const <AutoOpTxLine>[]) {
    if (t.band != null && band != null && t.band != band) continue;
    // In one period the own line comes before the decodes.
    entries.add((t.periodStartMs ?? 0, i++, AutoRow.tx(t)));
  }
  final decodes = view?.decodes ?? const <AutoOpDecode>[];
  for (final d in decodes) {
    final near = rx != null && d.df != null && (d.df! - rx).abs() <= rxWindowHz;
    if (near || d.toMe) entries.add((d.periodStartMs ?? 0, i++, AutoRow.decode(d)));
  }
  entries.sort((a, b) {
    final p = a.$1.compareTo(b.$1);
    return p != 0 ? p : a.$2.compareTo(b.$2);
  });
  return [for (final e in entries) e.$3];
}

class RxList extends StatelessWidget {
  const RxList({super.key});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final rows = rxRows(view);
    final rx = view?.station?.rxDf;
    return AutoCard(
      title: 'RX frequency · ${rx ?? '?'} Hz ±$rxWindowHz',
      child: rows.isEmpty
          ? Center(child: Text(view == null ? 'no data' : 'nothing near $rx Hz', style: AppTheme.mono(12, color: AppTheme.txtFaint)))
          // Reversed, so the newest line stays in view at the bottom. Shrink
          // wrapped at the top, so a short list starts at the top.
          : Align(
              alignment: Alignment.topCenter,
              child: ListView.builder(
                key: const ValueKey('auto-rx-list'),
                reverse: true,
                shrinkWrap: true,
                itemExtent: autoRowHeight,
                itemCount: rows.length,
                itemBuilder: (context, i) =>
                    AutoRowView(row: rows[rows.length - 1 - i], dxCall: view?.station?.dxCall, toMeTagOnly: true),
              ),
            ),
    );
  }
}
