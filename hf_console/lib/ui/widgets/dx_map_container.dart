// dx_map_container.dart — switches between the azimuthal compass and the
// Web-Mercator DX map, sharing the same DxSpotService data source.

import 'dart:async';

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../dxspot/dxspot_service.dart';
import '../../store/bus_store.dart';
import '../../store/selected_spot.dart';
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

/// Opening zoom of the VHF map module (≈ 400 km across on a tablet).
const double kVhfMapZoom = 7.0;

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

  /// Mercator only, no projection toggle (the VHF module).
  final bool mercatorOnly;

  /// Mercator draws [rotator]'s beam, aims it by tap and shows a STOP.
  final bool rotatorOverlay;

  const DxMapContainer({
    super.key,
    this.showPresets = true,
    this.rotator = hfRotator,
    this.initialProjection = DxProjection.azimuth,
    this.initialMercatorZoom,
    this.mercatorOnly = false,
    this.rotatorOverlay = false,
  });

  /// The VHF/UHF map module (CAM and UHF tabs): Mercator only, opening at
  /// zoom 7 on the station, the UHF az rotator's beam, tap-to-aim and an
  /// STOP for both sat axes. Zooms in to town level (up to 12) so a
  /// target ~30 km away can be picked out.
  const DxMapContainer.vhf({super.key, this.showPresets = true})
      : rotator = vhfRotator,
        initialProjection = DxProjection.mercator,
        initialMercatorZoom = kVhfMapZoom,
        mercatorOnly = true,
        rotatorOverlay = true;

  @override
  State<DxMapContainer> createState() => _DxMapContainerState();
}

class _DxMapContainerState extends State<DxMapContainer> {
  late DxProjection _projection = widget.mercatorOnly ? DxProjection.mercator : widget.initialProjection;

  // Aging tick for the dragon lift: the dragon must drop back to its corner
  // when the keyed selection expires (15 min), which on a quiet band happens
  // with no other rebuild. Mirrors CompassPanel's `_ageTick`.
  Timer? _ageTick;

  @override
  void initState() {
    super.initState();
    _ageTick = Timer.periodic(const Duration(seconds: 30), (_) {
      if (mounted) setState(() {});
    });
  }

  @override
  void dispose() {
    _ageTick?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final dx = context.watch<DxSpotService>();
    final store = context.watch<BusStore>();
    // When the selected-station chip is on screen (azimuth projection, keyed
    // selection not expired), the dragon parks on the chip's top edge instead
    // of its corner so the two don't collide.
    final chipVisible = _projection == DxProjection.azimuth &&
        selectedChipVisible(store.stateValue('muehle/hf/spots', 'selected'),
            DateTime.now().millisecondsSinceEpoch);
    return Stack(
      fit: StackFit.expand,
      children: [
        _projection == DxProjection.azimuth
            ? CompassPanel(showPresets: widget.showPresets, rotator: widget.rotator)
            : MercatorMapPanel(
                showPresets: widget.showPresets,
                rotator: widget.rotator,
                initialZoom: widget.initialMercatorZoom,
                rotatorOverlay: widget.rotatorOverlay,
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
            onProjectionChanged: widget.mercatorOnly ? null : (p) => setState(() => _projection = p),
          ),
        ),
        // Lower-left corner resident. Taps and drags must reach the map —
        // the dragon is decoration, not a control. Lifts above the
        // selected-station chip (which anchors at the same corner) when that
        // chip is on screen.
        AnimatedPositioned(
          duration: const Duration(milliseconds: 150),
          curve: Curves.easeInOut,
          left: 8,
          bottom: chipVisible ? 4 + kSelectedChipHeight + 4 : 4,
          child: IgnorePointer(child: const HorstKevin()),
        ),
      ],
    );
  }
}

class _MapChrome extends StatelessWidget {
  final DxProjection projection;
  final DxSpotFilter filter;
  /// Null hides the projection toggle (Mercator-only map).
  final ValueChanged<DxProjection>? onProjectionChanged;

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
        if (onProjectionChanged case final onChanged?) ...[
          const SizedBox(width: 6),
          _ProjectionToggle(
            projection: projection,
            onChanged: onChanged,
          ),
        ],
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
