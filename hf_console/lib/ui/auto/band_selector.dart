/// The band selector of the 75AUTO page. Display only in this work: each
/// band of the band table, with its value from the band survey of 75auto
/// (the points of the band, as the Value column of the 75auto window). A band without a batch in this session shows dimmed.
/// The column always lists the HF bands from 160 m to 10 m. A band that is
/// not in the band table of 75auto shows dimmed with no dial: 75auto does
/// not change to it.
///
/// A tap on a band of the table sends a signed `band` request to 75auto.
/// The rules change band when no QSO runs, so the tap is a request, not a
/// QSY: the requested band shows with an amber border until the radio is
/// on it. The tap needs the same gate as the other commands of the page.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_commands.dart';
import 'auto_live.dart';
import 'auto_tips.dart';

class BandSelector extends StatelessWidget {
  /// A horizontal strip for a portrait phone instead of a column.
  final bool horizontal;

  const BandSelector({super.key, this.horizontal = false});

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander>();
    return AutoTicker(builder: (context, live) => _build(context, cmd, live));
  }

  Widget _build(BuildContext context, AutoCommander cmd, AutoLive live) {
    final store = context.read<BusStore>();
    final view = store.autoOp;
    final entries = _entries(view, store.autoOpMeta?.bands);
    final current = view?.station?.band;
    final pending = view?.station?.bandChange?.target;
    final on = autoGateOf(store, live, keyStored: cmd.keyStored).operate == null;
    // The open request of this page: sent, not refused, and the radio not
    // yet on the band.
    final last = cmd.operate;
    final requested = last != null &&
            last.control == AutoControl.band &&
            last.sent != false &&
            !last.refused &&
            last.value != current
        ? last.value
        : null;
    final rows = [
      for (final (e, inTable) in entries)
        _BandRow(
          entry: e,
          inTable: inTable,
          current: e.band == current,
          pending: e.band != current &&
              ((pending != null && pending.isNotEmpty && e.band == pending) || e.band == requested),
          horizontal: horizontal,
          onTap: on && inTable && e.band != current ? () => cmd.send(AutoControl.band, e.band) : null,
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

  /// The HF bands that the column always lists, with their FT8 dial.
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

  /// The order of the bands, longest wavelength first. A band that is not
  /// in the list goes by its number in metres (cm last), after the list.
  static const _order = ['2200m', '630m', '160m', '80m', '60m', '40m', '30m', '20m', '17m', '15m', '12m', '10m', '6m', '4m', '2m'];

  static double _rank(String band) {
    final i = _order.indexOf(band);
    if (i >= 0) return i.toDouble();
    final m = RegExp(r'^(\d+(?:\.\d+)?)(c?m)$').firstMatch(band);
    if (m == null) return 1e9;
    final metres = double.parse(m.group(1)!) / (m.group(2) == 'cm' ? 100 : 1);
    // Longer wavelength first: a larger number gives a smaller rank.
    return _order.length + 1000 - metres;
  }

  /// The bands of the view and the fixed HF bands in band order, each
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
    // A fixed order by band, never by the dial in the view: a dial can be
    // missing or move inside its band, and the pills must not move.
    out.sort((a, b) => _rank(a.$1.band!).compareTo(_rank(b.$1.band!)));
    return out;
  }
}

class _BandRow extends StatelessWidget {
  final AutoOpBandCount entry;

  /// The band is in the band table of 75auto.
  final bool inTable;

  /// Sends the band request. Null: the pill does nothing.
  final VoidCallback? onTap;
  final bool current;
  final bool pending;
  final bool horizontal;

  const _BandRow({
    required this.entry,
    required this.inTable,
    required this.current,
    required this.pending,
    required this.horizontal,
    this.onTap,
  });

  @override
  Widget build(BuildContext context) {
    final band = entry.band!;
    final label = band.endsWith('m') && !band.endsWith('cm') ? band.substring(0, band.length - 1) : band;
    final seen = entry.seen;
    final off = seen == false || !inTable;
    final border = current ? AppTheme.accent : (pending ? AppTheme.amber : (off ? AppTheme.cardLine : AppTheme.cardLineHi));
    final text = current ? AppTheme.accent : (pending ? AppTheme.amber : (off ? AppTheme.txtFaint : AppTheme.txt));
    final tip = current
        ? '$band: the band of the radio now.'
        : inTable
            ? '$band: ${AutoControl.band.tip}'
            : '$band: not in the band table of 75auto. 75auto does not change to it.';
    return autoTip(tip, GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Container(
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
                  if (inTable) ...[
                    const SizedBox(width: 6),
                    // The value of the band survey, as the Value column of
                    // the 75auto window. A dash without one.
                    Text(entry.value == null ? '–' : '${entry.value}',
                        key: ValueKey('auto-band-value-$band'),
                        style: AppTheme.mono(12,
                            color: off || entry.value == null ? AppTheme.txtFaint : AppTheme.txt,
                            weight: FontWeight.w600)),
                  ],
                ],
              ),
            ),
          ),
          const SizedBox(width: 3),
        ],
      ),
    ),
    ));
  }
}
