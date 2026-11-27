/// The band selector of the 75AUTO page. Display only in this work: each
/// band of the band table in dial order, with the decode count of its latest
/// seen batch and the age of that count. A band without a batch in this
/// session shows `·`. The column always lists the HF bands from 160 m to
/// 10 m. A band that is not in the band table of 75auto shows dimmed with
/// `–`: 75auto does not change to it. A tap does nothing (a band change comes from a rule
/// proposal).
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';

class BandSelector extends StatelessWidget {
  /// A horizontal strip for a portrait phone instead of a column.
  final bool horizontal;

  const BandSelector({super.key, this.horizontal = false});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final metaBands = context.select<BusStore, List<String>?>((s) => s.autoOpMeta?.bands);
    final entries = _entries(view, metaBands);
    final current = view?.station?.band;
    final pending = view?.station?.bandChange?.target;
    final now = view?.nowMs;
    final rows = [
      for (final (e, inTable) in entries)
        _BandRow(
          entry: e,
          inTable: inTable,
          current: e.band == current,
          pending: pending != null && pending.isNotEmpty && e.band == pending && e.band != current,
          nowMs: now,
          horizontal: horizontal,
        ),
    ];
    if (horizontal) {
      return AutoCard(
        title: 'Band',
        shrink: true,
        child: SizedBox(
          height: 40,
          child: Row(children: [
            for (final r in rows) Expanded(child: Padding(padding: const EdgeInsets.all(2), child: r)),
          ]),
        ),
      );
    }
    return AutoCard(
      title: 'Band',
      child: Padding(
        padding: const EdgeInsets.all(3),
        child: LayoutBuilder(builder: (context, c) {
          // Share the height, up to 44 dp a band on a tall screen.
          final h = rows.isEmpty ? 0.0 : (c.maxHeight / rows.length).clamp(0.0, 44.0);
          return Column(children: [
            for (final r in rows)
              SizedBox(height: h, child: Padding(padding: const EdgeInsets.symmetric(vertical: 1.5), child: r)),
          ]);
        }),
      ),
    );
  }

  /// The HF bands that the column always lists, with their FT8 dial for
  /// the order.
  static const fixedBands = {
    '160m': 1840000,
    '80m': 3573000,
    '40m': 7074000,
    '30m': 10136000,
    '20m': 14074000,
    '17m': 18100000,
    '15m': 21074000,
    '12m': 24915000,
    '10m': 28074000,
  };

  /// The bands of the view and the fixed HF bands, sorted by dial, each
  /// with whether it is in the band table of 75auto. Without a view, the
  /// table is the bands of /meta, with no count. Without either, no rows.
  static List<(AutoOpBandCount, bool)> _entries(AutoOpView? view, List<String>? metaBands) {
    final bands = view?.bands;
    final List<AutoOpBandCount> table;
    if (bands != null && bands.isNotEmpty) {
      table = bands.where((b) => b.band != null).toList();
    } else {
      table = [for (final b in metaBands ?? const <String>[]) AutoOpBandCount(band: b, dialHz: fixedBands[b])];
    }
    if (table.isEmpty) return const [];
    final known = {for (final b in table) b.band!};
    final out = <(AutoOpBandCount, bool)>[
      for (final b in table) (b, true),
      for (final e in fixedBands.entries)
        if (!known.contains(e.key)) (AutoOpBandCount(band: e.key, dialHz: e.value), false),
    ];
    // 75auto sends its bands in dial order. Sort again with the fixed ones.
    int dial((AutoOpBandCount, bool) x) => x.$1.dialHz ?? fixedBands[x.$1.band] ?? 0;
    out.sort((a, b) => dial(a).compareTo(dial(b)));
    return out;
  }
}

class _BandRow extends StatelessWidget {
  final AutoOpBandCount entry;

  /// The band is in the band table of 75auto.
  final bool inTable;
  final bool current;
  final bool pending;
  final int? nowMs;
  final bool horizontal;

  const _BandRow({
    required this.entry,
    required this.inTable,
    required this.current,
    required this.pending,
    required this.nowMs,
    required this.horizontal,
  });

  @override
  Widget build(BuildContext context) {
    final band = entry.band!;
    final label = band.endsWith('m') && !band.endsWith('cm') ? band.substring(0, band.length - 1) : band;
    final seen = entry.seen;
    final String count;
    if (!inTable) {
      count = '–';
    } else if (seen == false) {
      count = '·';
    } else if (seen == true && entry.count != null) {
      count = '${entry.count}';
    } else {
      count = '?';
    }
    // The age of the count, from the start of its batch on the clock of
    // bwpc. Shown from one minute on.
    String? age;
    final ps = entry.periodStartMs;
    if (seen == true && ps != null && ps > 0 && nowMs != null) {
      final min = (nowMs! - ps) ~/ 60000;
      if (min >= 1) age = min >= 60 ? '${min ~/ 60}h' : '${min}m';
    }
    final off = seen == false || !inTable;
    final border = current ? AppTheme.accent : (pending ? AppTheme.amber : (off ? AppTheme.cardLine : AppTheme.cardLineHi));
    final text = current ? AppTheme.accent : (pending ? AppTheme.amber : (off ? AppTheme.txtFaint : AppTheme.txt));
    return Container(
      key: ValueKey('auto-band-$band'),
      decoration: BoxDecoration(
        color: current ? AppTheme.accentDim : AppTheme.pane,
        border: Border.all(color: border, width: current || pending ? 1.4 : 1),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        children: [
          // The band color, fixed across schemes (horstreporter palette).
          Container(
              key: ValueKey('auto-band-color-$band'),
              width: 4,
              color: inTable ? AppTheme.bandColor(band) : AppTheme.blend(AppTheme.bandColor(band), 0.3)),
          const SizedBox(width: 5),
          Expanded(
            child: FittedBox(
              fit: BoxFit.scaleDown,
              alignment: Alignment.centerLeft,
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.baseline,
                textBaseline: TextBaseline.alphabetic,
                children: [
                  Text(label, style: AppTheme.mono(13, color: text, weight: FontWeight.w700)),
                  const SizedBox(width: 6),
                  Text(count,
                      key: ValueKey('auto-band-count-$band'),
                      style: AppTheme.mono(11, color: off ? AppTheme.txtFaint : AppTheme.txtMute)),
                  if (age != null) ...[
                    const SizedBox(width: 2),
                    Text(age, style: AppTheme.mono(8, color: AppTheme.txtFaint)),
                  ],
                ],
              ),
            ),
          ),
          const SizedBox(width: 3),
        ],
      ),
    );
  }
}
