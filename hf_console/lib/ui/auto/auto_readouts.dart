/// The readout row of the 75AUTO page: band, dial, decodes, the DX station
/// with its QSO stage, and the transmit box with the slot bar.
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
      label: 'Band',
      value: st?.band ?? '?',
      sub: st?.mode ?? '?',
    );
    final dial = _Readout(
      label: 'Dial',
      value: st?.dialHz == null ? '?' : dialText(st!.dialHz!),
      sub: 'TX DF ${st?.txDf ?? '?'}',
    );
    final decodes = _Readout(
      label: 'Decodes',
      value: '${st?.decodeCount ?? '?'}',
      valueColor: st?.decodeCount == null ? null : AppTheme.green,
      sub: st?.lastDecodeEndMs == null ? 'last ?' : 'last ${utcHms(st!.lastDecodeEndMs!)}',
    );
    final dx = _DxReadout(station: st);
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
              gap,
              Expanded(flex: 3, child: decodes),
            ]),
          ),
          const SizedBox(height: 4),
          SizedBox(
            height: 56,
            child: Row(children: [Expanded(flex: 5, child: dx), gap, Expanded(flex: 4, child: tx)]),
          ),
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
        SizedBox(width: 112, child: decodes),
        gap,
        Expanded(child: dx),
        gap,
        SizedBox(width: 196, child: tx),
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
  final Color? valueColor;
  final String sub;

  const _Readout({required this.label, required this.value, this.valueColor, required this.sub});

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
                Text(value, maxLines: 1, style: AppTheme.mono(23, color: valueColor, weight: FontWeight.w600).copyWith(height: 1.1)),
                const SizedBox(width: 8),
                Text(sub, maxLines: 1, style: AppTheme.mono(11, color: AppTheme.txtMute)),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

/// The DX station: call, entity, try or repeat count, and the QSO stage.
class _DxReadout extends StatelessWidget {
  final AutoOpStation? station;

  const _DxReadout({required this.station});

  @override
  Widget build(BuildContext context) {
    final st = station;
    final call = st?.dxCall;
    final parts = <String>['DX call'];
    final entity = st?.dxEntity;
    if (entity != null && entity.isNotEmpty) parts.add(entity);
    final rpt = st?.repeatCount ?? 0;
    final tries = st?.tryCount ?? 0;
    if (rpt > 0) {
      parts.add('rpt $rpt');
    } else if (tries > 0) {
      parts.add('try $tries');
    }
    final String value;
    final Color color;
    if (call == null) {
      value = '?';
      color = AppTheme.txtMute;
    } else if (call.isEmpty) {
      value = 'no DX';
      color = AppTheme.txtFaint;
    } else {
      value = call;
      color = AppTheme.accent;
    }
    final stage = st?.qsoStage;
    final now = stage == null ? -1 : qsoStages.indexOf(stage);
    return _Box(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text(parts.join(' · ').toUpperCase(), maxLines: 1, overflow: TextOverflow.ellipsis, style: _labelStyle()),
          FittedBox(
            fit: BoxFit.scaleDown,
            alignment: Alignment.centerLeft,
            child: Row(
              children: [
                Text(value, maxLines: 1, style: AppTheme.mono(24, color: color, weight: FontWeight.w700).copyWith(height: 1.1)),
                const SizedBox(width: 10),
                for (var i = 0; i < qsoStages.length; i++) ...[
                  _StageChip(qsoStages[i], done: now >= 0 && i < now, current: i == now),
                  const SizedBox(width: 3),
                ],
              ],
            ),
          ),
        ],
      ),
    );
  }
}

class _StageChip extends StatelessWidget {
  final String label;
  final bool done;
  final bool current;

  const _StageChip(this.label, {required this.done, required this.current});

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

/// The transmit box: TX or RX, the parity of the own transmission, and the
/// slot bar. Live TX is red. The bar runs on the local clock from
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
        final parity = switch (st?.txParity) {
          'even' => 'EVEN',
          'odd' => 'ODD',
          '' => 'no TX yet',
          _ => 'parity ?',
        };
        final sub = stopUnconfirmed
            ? 'WSJT-X STILL SENDING'
            : st?.txEnabled == false
                ? '$parity · TX OFF'
                : parity;
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
              Text('TRANSMIT · SLOT', maxLines: 1, style: _labelStyle()),
              FittedBox(
                fit: BoxFit.scaleDown,
                alignment: Alignment.centerLeft,
                child: Row(
                  crossAxisAlignment: CrossAxisAlignment.baseline,
                  textBaseline: TextBaseline.alphabetic,
                  children: [
                    Text(value, key: const ValueKey('auto-tx'), style: AppTheme.mono(20, color: valueColor, weight: FontWeight.w700).copyWith(height: 1.1)),
                    const SizedBox(width: 12),
                    Text(sub,
                        style: AppTheme.mono(12,
                            color: stopUnconfirmed ? AppTheme.red : AppTheme.txtMute,
                            weight: stopUnconfirmed ? FontWeight.w700 : FontWeight.w400)),
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
