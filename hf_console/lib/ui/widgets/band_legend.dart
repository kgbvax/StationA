// band_legend.dart — the band key shared by both map projections.
//
// One chip per band currently present in the spot feed, ordered by canonical
// band (lowest frequency first). Chip colors come from `AppTheme.bandColor`
// (the horstreporter palette; see `docs/conventions/band-mode-reference.md`).
// Extracted from the compass panel so the Mercator map can carry the same
// key — on the UHF page the feed is narrowed server-side to 2m/70cm, and the
// legend is what makes that contract visible to the operator.

import 'package:flutter/material.dart';

import '../../dxspot/dxspot_service.dart';
import '../theme.dart';

/// Canonical band display order: HF lowest→highest, then VHF/UHF. Every band
/// listed here must exist in `AppTheme.bandColor` — anything else would
/// render the grey fallback and contradict the key.
const List<String> kBandOrder = [
  '160m', '80m', '60m', '40m', '30m', '20m', '17m', '15m', '12m', '10m', '6m', '2m', '70cm',
];

/// The bands present in [spots], in [kBandOrder] order.
List<String> visibleBands(List<DxSpot> spots) {
  final set = <String>{};
  for (final s in spots) {
    if (s.band.isNotEmpty) set.add(s.band);
  }
  return kBandOrder.where(set.contains).toList(growable: false);
}

/// Chip rail showing only the bands currently in use. Fades in when banded
/// spots arrive, out when they leave. [vertical] stacks the chips for a rail
/// pinned to a card edge; the default wraps centered (the footer-row look).
class BandLegend extends StatelessWidget {
  final List<String> visible;
  final bool vertical;
  const BandLegend({super.key, required this.visible, this.vertical = false});

  @override
  Widget build(BuildContext context) {
    return AnimatedSwitcher(
      duration: const Duration(milliseconds: 180),
      switchInCurve: Curves.easeOut,
      switchOutCurve: Curves.easeIn,
      transitionBuilder: (child, anim) =>
          FadeTransition(opacity: anim, child: child),
      child: visible.isEmpty
          ? const SizedBox.shrink(key: ValueKey('band-legend-empty'))
          : Padding(
              key: ValueKey('band-legend-${visible.join('-')}-$vertical'),
              padding: const EdgeInsets.only(right: 6),
              child: vertical
                  ? Column(
                      mainAxisSize: MainAxisSize.min,
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        for (final b in visible) Padding(
                          padding: const EdgeInsets.symmetric(vertical: 2),
                          child: _BandChip(b),
                        ),
                      ],
                    )
                  : Wrap(
                      spacing: 4,
                      runSpacing: 4,
                      alignment: WrapAlignment.center,
                      children: [
                        for (final b in visible) _BandChip(b),
                      ],
                    ),
            ),
    );
  }
}

class _BandChip extends StatelessWidget {
  final String band;
  const _BandChip(this.band);

  @override
  Widget build(BuildContext context) {
    final color = AppTheme.bandColor(band);
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border.all(color: AppTheme.cardLineHi),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Container(width: 7, height: 7, decoration: BoxDecoration(color: color, shape: BoxShape.rectangle)),
          const SizedBox(width: 5),
          Text(band, style: AppTheme.mono(10, weight: FontWeight.w700)),
        ],
      ),
    );
  }
}
