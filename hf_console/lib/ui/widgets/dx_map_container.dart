// dx_map_container.dart — switches between the azimuthal compass and the
// Web-Mercator DX map, sharing the same DxSpotService data source.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../dxspot/dxspot_service.dart';
import '../theme.dart';
import 'compass_panel.dart';
import 'mercator_map_panel.dart';

enum DxProjection { azimuth, mercator }

class DxMapContainer extends StatefulWidget {
  /// Whether the map panels overlay the direction-preset rail on their right
  /// edge (tablet). Phones pass false and keep the horizontal presets bar in
  /// their scrolling controls column instead — the phone map is too small to
  /// carry a five-button rail.
  final bool showPresets;

  const DxMapContainer({super.key, this.showPresets = true});

  @override
  State<DxMapContainer> createState() => _DxMapContainerState();
}

class _DxMapContainerState extends State<DxMapContainer> {
  DxProjection _projection = DxProjection.azimuth;

  @override
  Widget build(BuildContext context) {
    final dx = context.watch<DxSpotService>();
    return Stack(
      fit: StackFit.expand,
      children: [
        _projection == DxProjection.azimuth
            ? CompassPanel(showPresets: widget.showPresets)
            : MercatorMapPanel(showPresets: widget.showPresets),
        Positioned(
          top: 8,
          right: 8,
          child: _MapChrome(
            projection: _projection,
            filter: dx.filter,
            onProjectionChanged: (p) => setState(() => _projection = p),
          ),
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
