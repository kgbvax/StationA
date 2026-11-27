/// The readout row of the 75AUTO page: mode (band and digimode), dial,
/// and the transmit box with the slot bar. The DX station is the DX card.
///
/// A missing value shows `?`. The page never puts a definite value in the
/// place of missing data.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_live.dart';

/// The QSO stages in order (contract §5.3, `station.qso_stage`).
const qsoStages = ['CALL', 'RPT', 'R+RPT', 'RR73', 'LOG'];

class AutoReadouts extends StatelessWidget {
  /// Two rows for a portrait phone instead of one row.
  final bool twoRows;

  const AutoReadouts({super.key, this.twoRows = false});

  @override
  Widget build(BuildContext context) {
    final st = context.select<BusStore, AutoOpStation?>((s) => s.autoOp?.station);
    const gap = SizedBox(width: 4);
    final band = _Readout(
      label: 'Mode',
      value: st?.band ?? '?',
      sub: st?.mode ?? '?',
    );
    final dial = _Readout(
      label: 'Dial',
      value: st?.dialHz == null ? '?' : dialText(st!.dialHz!),
      sub: 'TX DF ${st?.txDf ?? '?'}',
    );
    final tx = _TransmitReadout(station: st);
    if (twoRows) {
      return Column(
        children: [
          SizedBox(
            height: 56,
            child: Row(children: [
              Expanded(flex: 2, child: band),
              gap,
              Expanded(flex: 4, child: dial),
            ]),
          ),
          const SizedBox(height: 4),
          SizedBox(height: 56, child: tx),
        ],
      );
    }
    return Row(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        SizedBox(width: 92, child: band),
        gap,
        SizedBox(width: 196, child: dial),
        gap,
        Expanded(child: tx),
      ],
    );
  }
}

TextStyle _labelStyle() => AppTheme.display(11, color: AppTheme.txtMute, letterSpacing: 0.1).copyWith(height: 1.15);

class _Box extends StatelessWidget {
  final Widget child;
  final Color? border;

  const _Box({required this.child, this.border});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.fromLTRB(10, 3, 10, 4),
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border.all(color: border ?? AppTheme.cardLine, width: border == null ? 1 : 1.4),
      ),
      child: child,
    );
  }
}

class _Readout extends StatelessWidget {
  final String label;
  final String value;

  /// The small text after the value. Null: none.
  final String? sub;

  const _Readout({required this.label, required this.value, this.sub});

  @override
  Widget build(BuildContext context) {
    return _Box(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text(label.toUpperCase(), maxLines: 1, style: _labelStyle()),
          FittedBox(
            fit: BoxFit.scaleDown,
            alignment: Alignment.centerLeft,
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.baseline,
              textBaseline: TextBaseline.alphabetic,
              children: [
                Text(value, maxLines: 1, style: AppTheme.mono(23, weight: FontWeight.w600).copyWith(height: 1.1)),
                if (sub != null) ...[
                  const SizedBox(width: 8),
                  Text(sub!, maxLines: 1, style: AppTheme.mono(11, color: AppTheme.txtMute)),
                ],
              ],
            ),
          ),
        ],
      ),
    );
  }
}

/// The DX station: call, entity, try or repeat count, and the QSO stage.
/// One QSO stage chip: done green, current filled accent.
class AutoStageChip extends StatelessWidget {
  final String label;
  final bool done;
  final bool current;

  const AutoStageChip(this.label, {super.key, required this.done, required this.current});

  @override
  Widget build(BuildContext context) {
    final color = current ? AppTheme.accent : (done ? AppTheme.green : AppTheme.cardLineHi);
    return Container(
      key: current ? const ValueKey('auto-stage-now') : null,
      padding: const EdgeInsets.symmetric(horizontal: 4, vertical: 1),
      decoration: BoxDecoration(
        color: current ? AppTheme.accent : null,
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(3),
      ),
      child: Text(
        label,
        style: AppTheme.mono(10,
            color: current ? AppTheme.activeButtonText : (done ? AppTheme.green : AppTheme.txtFaint),
            weight: FontWeight.w600),
      ),
    );
  }
}

/// The transmit box: TX or RX, a warning while WSJT-X still sends after a
/// stop, and the slot bar. Live TX is red. The bar runs on the local clock from
/// `slot_offset_ms` and freezes, faint, while the data is not fresh.
class _TransmitReadout extends StatelessWidget {
  final AutoOpStation? station;

  const _TransmitReadout({required this.station});

  @override
  Widget build(BuildContext context) {
    final st = station;
    final stopUnconfirmed = context.select<BusStore, bool?>((s) => s.autoOp?.stopUnconfirmed) == true;
    final reduced = MediaQuery.maybeDisableAnimationsOf(context) ?? false;
    return AutoTicker(
      period: Duration(milliseconds: reduced ? 1000 : 100),
      builder: (context, live) {
        final tx = st?.transmitting;
        final lit = tx == true && live.live;
        final String value;
        final Color valueColor;
        if (tx == null) {
          value = 'TX ?';
          valueColor = AppTheme.txtMute;
        } else if (tx) {
          value = 'TX';
          valueColor = live.live ? AppTheme.red : AppTheme.blend(AppTheme.red, 0.55);
        } else {
          value = 'RX';
          valueColor = AppTheme.txtMute;
        }
        var frac = 0.0;
        final off = live.slotOffsetMs;
        final period = live.periodMs;
        if (off != null && period != null && period > 0) {
          final ms = reduced ? off - off % 1000 : off;
          frac = (ms / period).clamp(0.0, 1.0);
        }
        final barColor = !live.live ? AppTheme.txtFaint : (tx == true ? AppTheme.red : AppTheme.accent);
        return _Box(
          border: lit ? AppTheme.red : null,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              Text('TRANSMIT', maxLines: 1, style: _labelStyle()),
              FittedBox(
                fit: BoxFit.scaleDown,
                alignment: Alignment.centerLeft,
                child: Row(
                  crossAxisAlignment: CrossAxisAlignment.baseline,
                  textBaseline: TextBaseline.alphabetic,
                  children: [
                    Text(value, key: const ValueKey('auto-tx'), style: AppTheme.mono(20, color: valueColor, weight: FontWeight.w700).copyWith(height: 1.1)),
                    // No "TX OFF": RX already says it.
                    if (stopUnconfirmed) ...[
                      const SizedBox(width: 12),
                      Text('WSJT-X STILL SENDING',
                          style: AppTheme.mono(12, color: AppTheme.red, weight: FontWeight.w700)),
                    ],
                  ],
                ),
              ),
              Container(
                key: const ValueKey('auto-slot-bar'),
                height: 7,
                decoration: BoxDecoration(
                  color: AppTheme.pane,
                  border: Border.all(color: AppTheme.cardLineHi),
                  borderRadius: BorderRadius.circular(2),
                ),
                alignment: Alignment.centerLeft,
                child: FractionallySizedBox(
                  widthFactor: frac,
                  heightFactor: 1,
                  child: ColoredBox(color: barColor),
                ),
              ),
            ],
          ),
        );
      },
    );
  }
}
