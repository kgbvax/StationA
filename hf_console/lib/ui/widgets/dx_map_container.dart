// dx_map_container.dart — switches between the azimuthal compass and the
// Web-Mercator DX map, sharing the same DxSpotService data source.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../dxspot/dxspot_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'compass_panel.dart';
import 'mercator_map_panel.dart';

enum DxProjection { azimuth, mercator }

/// Horst-Kevin — band-heckling dragon, lower-left corner of the map card on
/// both projections. Bundled from horstreporter's `static/hk.jpg`; the PNG
/// variant (`assets/img/hk-removebg.png`) is the same photo with its
/// background removed so it composites cleanly against the map. The 56 dp
/// circle-clip + accent ring used in the first pass clipped the dragon's
/// horns; the alpha-clean PNG lets us render him as-is, so the widget just
/// paints the image with a tooltip.
class HorstKevin extends StatelessWidget {
  /// Target rendered height in logical pixels. Width is computed from the
  /// source's 482:517 aspect ratio (≈ 0.93) so the dragon stays proportional.
  static const double height = 64;

  const HorstKevin({super.key});

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: 'Horst-Kevin — band-heckling dragon',
      child: Image.asset(
        'assets/img/hk-removebg.png',
        height: height,
        fit: BoxFit.contain,
        // PaintingBinding's default image cache keeps the PNG in memory after
        // first decode. `filterQuality: medium` (not high) is sufficient for
        // this small fixed-size asset — high wastes CPU on every layout.
        filterQuality: FilterQuality.medium,
      ),
    );
  }
}

class DxMapContainer extends StatefulWidget {
  /// Whether the map panels overlay the direction-preset rail on their right
  /// edge (tablet). Phones pass false and keep the horizontal presets bar in
  /// their scrolling controls column instead — the phone map is too small to
  /// carry a five-button rail.
  final bool showPresets;

  /// Which rotator the compass dial reads — `null` renders the bare DX map
  /// with no rotator needle or aim affordances (Station page).
  final RotatorSurface? rotator;

  /// Projection the map opens with. The UHF page passes [DxProjection.mercator]
  /// — VHF DX is read on the pannable world map, not the QTH-centred dial.
  final DxProjection initialProjection;

  /// Zoom the Mercator view opens with; `null` uses the panel default.
  final double? initialMercatorZoom;

  const DxMapContainer({
    super.key,
    this.showPresets = true,
    this.rotator = hfRotator,
    this.initialProjection = DxProjection.azimuth,
    this.initialMercatorZoom,
  });

  @override
  State<DxMapContainer> createState() => _DxMapContainerState();
}

class _DxMapContainerState extends State<DxMapContainer> {
  late DxProjection _projection = widget.initialProjection;

  @override
  Widget build(BuildContext context) {
    final dx = context.watch<DxSpotService>();
    return Stack(
      fit: StackFit.expand,
      children: [
        _projection == DxProjection.azimuth
            ? CompassPanel(showPresets: widget.showPresets, rotator: widget.rotator)
            : MercatorMapPanel(
                showPresets: widget.showPresets,
                rotator: widget.rotator,
                initialZoom: widget.initialMercatorZoom,
              ),
        // Top-left: the compass panel's own chrome (zoom badge, azimuth
        // chip) owns the top-right corner — overlaying the filter/projection
        // chrome there drew one on top of the other.
        Positioned(
          top: 8,
          left: 8,
          child: _MapChrome(
            projection: _projection,
            filter: dx.filter,
            onProjectionChanged: (p) => setState(() => _projection = p),
          ),
        ),
        // Lower-left corner resident. Taps and drags must reach the map —
        // the dragon is decoration, not a control.
        Positioned(
          left: 8,
          bottom: 4,
          child: IgnorePointer(child: const HorstKevin()),
        ),
      ],
    );
  }
}

class _MapChrome extends StatelessWidget {
  final DxProjection projection;
  final DxSpotFilter filter;
  final ValueChanged<DxProjection> onProjectionChanged;

  const _MapChrome({
    required this.projection,
    required this.filter,
    required this.onProjectionChanged,
  });

  @override
  Widget build(BuildContext context) {
    final threshold = filter.threshold;
    final filterLabel = threshold == null
        ? 'SNR off'
        : '${filter.mode.toUpperCase()} ≥ ${threshold}dB';

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Container(
          padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
          decoration: BoxDecoration(
            color: AppTheme.card.withValues(alpha: 0.85),
            borderRadius: BorderRadius.circular(4),
            border: Border.all(color: AppTheme.cardLine),
          ),
          child: Text(
            filterLabel,
            style: AppTheme.body(12, color: AppTheme.txt, weight: FontWeight.w500),
          ),
        ),
        const SizedBox(width: 6),
        _ProjectionToggle(
          projection: projection,
          onChanged: onProjectionChanged,
        ),
      ],
    );
  }
}

class _ProjectionToggle extends StatelessWidget {
  final DxProjection projection;
  final ValueChanged<DxProjection> onChanged;

  const _ProjectionToggle({
    required this.projection,
    required this.onChanged,
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 4, vertical: 4),
      decoration: BoxDecoration(
        color: AppTheme.card.withValues(alpha: 0.85),
        borderRadius: BorderRadius.circular(4),
        border: Border.all(color: AppTheme.cardLine),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          _ToggleButton(
            icon: Icons.explore,
            selected: projection == DxProjection.azimuth,
            onTap: () => onChanged(DxProjection.azimuth),
            tooltip: 'Azimuthal',
          ),
          _ToggleButton(
            icon: Icons.map,
            selected: projection == DxProjection.mercator,
            onTap: () => onChanged(DxProjection.mercator),
            tooltip: 'Mercator',
          ),
        ],
      ),
    );
  }
}

class _ToggleButton extends StatelessWidget {
  final IconData icon;
  final bool selected;
  final VoidCallback onTap;
  final String tooltip;

  const _ToggleButton({
    required this.icon,
    required this.selected,
    required this.onTap,
    required this.tooltip,
  });

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: tooltip,
      child: GestureDetector(
        onTap: onTap,
        child: Container(
          padding: const EdgeInsets.all(4),
          decoration: BoxDecoration(
            color: selected ? AppTheme.accent.withValues(alpha: 0.25) : Colors.transparent,
            borderRadius: BorderRadius.circular(3),
          ),
          child: Icon(
            icon,
            size: 18,
            color: selected ? AppTheme.accent : AppTheme.txtMute,
          ),
        ),
      ),
    );
  }
}
