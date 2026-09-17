import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'dart:async';
import 'dart:math' as math;
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/selected_spot.dart';
import '../../store/wiring.dart';
import '../../dxspot/dxspot_service.dart';
import '../../dxspot/projection.dart';
import '../../dxspot/world_geometry.dart';
import '../theme.dart';
import 'rotator_presets_bar.dart';
import 'world_layer_cache.dart';

class CompassPanel extends StatefulWidget {
  /// Whether to overlay the direction-preset rail on the right card edge
  /// (tablet layout; phones keep the horizontal bar in their scroll column).
  final bool showPresets;

  /// Which rotator the dial reads. `null` renders the bare DX compass —
  /// spots, world, band key, zoom — with no needle, azimuth chip, presets
  /// or aim affordances (Station page).
  final RotatorSurface? rotator;

  const CompassPanel({super.key, this.showPresets = true, this.rotator = hfRotator});

  @override
  State<CompassPanel> createState() => _CompassPanelState();
}

/// Compass zoom bounds — match horstreporter's `clampAzimuthZoom` in
/// `static/azimuth-runtime.js:265-267` (zoom ∈ [1.0, 5.0], default 1.5) so the
/// Android console matches the web frontend's starting view.
const double kCompassZoomMin = 1.0;
const double kCompassZoomMax = 5.0;
const double kCompassZoomDefault = 1.5;

/// Step size for the +/- zoom buttons, matching horstreporter's
/// `app.js:1472-1488` (±0.2 per click). Pure helper so it's testable without
/// a `WidgetTester`.
const double kCompassZoomStep = 0.2;

/// Inset the painter leaves between the SizedBox edge and the painted disc
/// circle (kept as a top-level const so the layout's `SizedBox` can shrink
/// by the same amount and chrome positioned at `Positioned(right:)` /
/// `Positioned(left:)` lands on the *visible* disc rim, not past it).
const double kDiscInset = 10.0;

/// The selected-station chip's height (its `minHeight` — the content is one
/// line and never outgrows it). Top-level so DxMapContainer can park the
/// dragon exactly on the chip's top edge without hard-coding the same 48.
const double kSelectedChipHeight = 48.0;

double clampCompassZoom(double z) {
  // NaN falls back to the default; ±infinity clamps to the corresponding
  // bound (otherwise `z.clamp(min, max)` returns the same infinity, which
  // then poisons every comparison downstream).
  if (z.isNaN) return kCompassZoomDefault;
  if (z.isInfinite) return z > 0 ? kCompassZoomMax : kCompassZoomMin;
  return z.clamp(kCompassZoomMin, kCompassZoomMax);
}

/// Return the next zoom value when the user taps +/-. Wraps [clampCompassZoom]
/// so a tap at the bound is a no-op (returns the same value). Tolerance
/// `1e-9` matches horstreporter's `<= MIN + 1e-6` disable check in
/// `app.js:254, 259`.
double zoomStep(double current, {required bool up}) =>
    clampCompassZoom(current + (up ? kCompassZoomStep : -kCompassZoomStep));

/// Whether the `+` zoom button should be enabled at [z].
bool canZoomIn(double z) => z < kCompassZoomMax - 1e-9;

/// Whether the `-` zoom button should be enabled at [z].
bool canZoomOut(double z) => z > kCompassZoomMin + 1e-9;

class _CompassPanelState extends State<CompassPanel> {
  double _zoom = kCompassZoomDefault;
  // World landmass raster cache — lives across rebuilds so a zoom drag reuses
  // the projected bitmap while (cx, cy, r, zoom, center) don't change. Owned
  // here (not on the painter) because the painter is reconstructed every
  // frame. Disposed in `dispose()`.
  final WorldLayerCache _world = WorldLayerCache();
  // Aging tick for the selected-station marker: the marker dims/greys out as
  // the keyed call grows old, which only repaints if something rebuilds the
  // panel. A quiet band produces no other rebuilds, so keep a slow tick.
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
    _world.dispose();
    super.dispose();
  }

  void _setZoom(double z) {
    final clamped = clampCompassZoom(z);
    if (clamped == _zoom) return;
    setState(() => _zoom = clamped);
  }

  @override
  Widget build(BuildContext context) {
    return _CompassBody(
      zoom: _zoom,
      onZoomChanged: _setZoom,
      world: _world,
      showPresets: widget.showPresets,
      rotator: widget.rotator,
    );
  }
}

class _CompassBody extends StatelessWidget {
  final double zoom;
  final ValueChanged<double> onZoomChanged;
  final WorldLayerCache world;
  final bool showPresets;
  final RotatorSurface? rotator;
  const _CompassBody({
    required this.zoom,
    required this.onZoomChanged,
    required this.world,
    required this.showPresets,
    required this.rotator,
  });

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();
    final dx = context.watch<DxSpotService>();

    final surface = rotator;
    final rotatorSlot = surface?.stateSlot;
    final rotatorBridge = rotatorSlot == null ? null : store.slots[rotatorSlot];
    final rotatorOnline = surface != null && (rotatorBridge?.isOnline ?? false) && store.linkUp;
    final az = rotatorSlot == null
        ? 0.0
        : store.stateValueAs<num>(rotatorSlot, 'az')?.toDouble() ?? 0.0;
    final targetAz = rotatorSlot == null
        ? az
        : store.stateValueAs<num>(rotatorSlot, surface!.targetKey)?.toDouble() ?? az;
    final moving = rotatorSlot != null &&
        (store.stateValueAs<bool>(rotatorSlot, 'moving') ?? false);

    // ant-ctrl's forward/reverse beam flip is an HF-stack concept; the VHF
    // dial has no counterpart and always reads forward.
    final direction = surface?.stateSlot == hfRotator.stateSlot
        ? store.stateValueAs<String>('muehle/hf/ant-ctrl', 'direction') ?? 'forward'
        : 'forward';

    // The station the operator keyed in the shack logger (DXLog/Log4OM),
    // published by logger-spot-bridge on muehle/hf/spots. Rendered as a pin
    // (coordinates) or a bearing ray (azimuth only) plus a bottom-left
    // read-out chip — the "is turning the beam worth it" aid.
    final selected = SelectedSpot.fromSelected(store.stateValue('muehle/hf/spots', 'selected'));
    final nowMs = DateTime.now().millisecondsSinceEpoch;
    final selectedAge = selected?.ageSecondsAt(nowMs) ?? 0;
    final selectedLive = selected != null && stalenessFor(selectedAge) != SelectedStaleness.expired;
    // Tap-to-aim on the chip is offered only when it can actually work:
    // rotor online and a bearing derived (a pin from coordinates has its
    // beam answer filled by the bridge; a logger azimuth counts too).
    final selectedAimable = selectedLive && rotatorOnline && selected.azimuth != null;

    final targetDiff = (targetAz - az).abs();
    final targetVisible = targetDiff > 5.0;

    final azimuthParts = <String>[
      '${az.round()}°',
      if (rotatorOnline && targetVisible) '→ ${targetAz.round()}°',
      if (moving) 'MOVING',
      if (!rotatorOnline) 'OFFLINE',
    ];
    final azimuthColor = moving
        ? AppTheme.red
        : !rotatorOnline
            ? AppTheme.txtMute
            : AppTheme.accent;

    void sendAz(double value) {
      final surface = rotator;
      if (surface == null) return;
      mqtt.publish(
        cmdTopic(surface.cmdSlot),
        surface.aimPayload(value),
        retain: cmdRetain[surface.stateSlot]!,
      );
    }

    // The disc itself keeps a small symmetric inset from the card edges so
    // it doesn't touch the pane border. The floating chrome is positioned
    // relative to the card edges, not the disc, so changing the disc size or
    // centering never shifts the band key, azimuth pill, or zoom buttons.
    return LayoutBuilder(
      builder: (context, constraints) {
        // The panel is allowed to fill the parent card to its edges. The disc
        // itself is centered and given a small inset so it doesn't touch the
        // pane border, but the overlay chrome uses the full panel bounds and
        // is positioned relative to the card edges.
        final maxW = constraints.maxWidth;
        final maxH = constraints.maxHeight;

        // Space reserved around the disc for the overlay chrome.
        const chromePadding = EdgeInsets.fromLTRB(8, 6, 8, 6);
        final innerW = (maxW - chromePadding.horizontal).clamp(0.0, double.infinity);
        final innerH = (maxH - chromePadding.vertical).clamp(0.0, double.infinity);

        // Disc is size-bounded by the smaller of the inner width / height
        // (×1.15 to bias slightly wider so the disc fills more of the pane
        // when it's wider than tall). The SizedBox itself is shrunk by
        // `2 * kDiscInset` on each axis so its bounds match the painter's
        // visible disc circle (the painter draws the disc at radius =
        // half-side − kDiscInset, leaving a transparent margin around it).
        final size = math.min(innerW, innerH * 1.15);
        final discSize = size - 2 * kDiscInset;

        final discWidth = discSize.clamp(0.0, innerW);
        final discHeight = discSize.clamp(0.0, innerH);
        final discPaintR = math.min(discWidth, discHeight) / 2.0 - kDiscInset;

        // The interactive compass disc, centered inside the module with a
        // small inset so it stays clear of the floating chrome.
        final disc = SizedBox(
          width: discWidth,
          height: discHeight,
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTapUp: rotatorOnline
                ? (details) {
                    final local = details.localPosition;
                    final cx = discWidth / 2;
                    final cy = discHeight / 2;
                    final dx = local.dx - cx;
                    final dy = local.dy - cy;
                    final deg = math.atan2(dy, dx) * 180 / math.pi;
                    final tappedAz = ((deg + 90) % 360 + 360) % 360;
                    sendAz(tappedAz);
                  }
                : null,
            // Vertical drag on the right gutter of the disc adjusts zoom.
            // Drag up = zoom in, drag down = zoom out — matches
            // horstreporter's wheel-zoom polarity. The left / inside-disc
            // region keeps tap-to-aim for the rotator, and only drags that
            // *start* in the right gutter take over zoom. The +/- zoom
            // buttons overlaid bottom-right are the discoverable entry
            // point; this drag is kept as a power-user shortcut.
            onVerticalDragStart: dx.active
                ? (details) {
                    final local = details.localPosition;
                    final cx = discWidth / 2;
                    final cy = discHeight / 2;
                    if (!_isRightGutter(local, cx, cy, discPaintR)) return;
                  }
                : null,
            onVerticalDragUpdate: dx.active
                ? (details) {
                    // ~30 px of vertical drag = +1.0 zoom step.
                    const pxPerZoom = 30.0;
                    final dz = -details.primaryDelta! / pxPerZoom;
                    onZoomChanged(zoom + dz);
                  }
                : null,
            // Load the bundled coastline GeoJSON once at app startup (cached
            // in `WorldGeometry.instance`); only the painter gets the rings
            // when the overlay is active. While the load is in flight we
            // render the beam-only compass — same UX as before.
            child: FutureBuilder<List<List<LatLng>>>(
              future: dx.active ? WorldGeometry.instance.load() : null,
              builder: (context, snap) {
                return RepaintBoundary(
                  child: CustomPaint(
                    painter: _CompassPainter(
                      az: az,
                      direction: direction,
                      targetAz: targetAz,
                      showBeam: rotator != null,
                      gridSquares: dx.gridSquares,
                      centerLat: dx.centerLat,
                      centerLng: dx.centerLng,
                      worldRings: snap.data,
                      zoom: zoom,
                      world: world,
                      selected: selectedLive ? selected : null,
                      selectedAgeSeconds: selectedAge,
                    ),
                  ),
                );
              },
            ),
          ),
        );

        final visibleBands = _BandLegend.visibleBands(dx.spots);

        return Stack(
          fit: StackFit.expand,
          clipBehavior: Clip.none,
          children: [
            // Layer 1: the disc, inset from the card edges so it doesn't touch
            // the pane border. The Padding is applied only to the disc, not to
            // the overlay chrome — this is the key to keeping the band key,
            // azimuth pill and zoom buttons at the card edge.
            Center(
              child: Padding(
                padding: chromePadding,
                child: disc,
              ),
            ),
            // Layer 2: top-row chrome, small margin from the card edge.
            Positioned(
              top: 4,
              right: 8,
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                mainAxisSize: MainAxisSize.min,
                children: [
                  _ZoomBadge(zoom: zoom),
                  // No rotator surface (Station page): no azimuth read-out —
                  // the compass is a plain DX map there.
                  if (rotator != null) ...[
                    const SizedBox(width: 6),
                    IgnorePointer(
                      child: _AzimuthChip(
                        parts: azimuthParts,
                        color: azimuthColor,
                      ),
                    ),
                  ],
                ],
              ),
            ),
            // Layer 3: left band-key rail, hugging the left card edge and
            // vertically centered on the module.
            if (visibleBands.isNotEmpty)
              Positioned(
                left: 8,
                top: 40,
                bottom: 40,
                child: IgnorePointer(
                  child: Align(
                    alignment: Alignment.centerLeft,
                    child: _BandLegend(
                      visible: visibleBands,
                      vertical: true,
                    ),
                  ),
                ),
              ),
            // Layer 4: +/- zoom stepper, bottom-right of the card.
            Positioned(
              right: 4,
              bottom: 4,
              child: _ZoomStepper(
                zoom: zoom,
                onZoomChanged: onZoomChanged,
              ),
            ),
            // Layer 4b: selected-station read-out, bottom-left. Shows what
            // the operator keyed in the logger and the beam answer — the
            // "worth turning?" decision aid, and the actuator: tapping the
            // chip aims the rotor at the keyed station (the same set_az the
            // disc's tap-to-aim publishes). Inert when the rotor is offline
            // or the bridge derived no bearing. Hidden when nothing is keyed
            // (or the marker aged out).
            if (selectedLive)
              Positioned(
                left: 8,
                bottom: 4,
                child: _SelectedChip(
                  parts: _selectedChipParts(selected, selectedAge, aimable: selectedAimable),
                  stale: stalenessFor(selectedAge) == SelectedStaleness.stale,
                  onTap: selectedAimable ? () => sendAz(selected.azimuth!) : null,
                ),
              ),
            // Layer 5: direction presets, stacked directly above the zoom
            // stepper on the right card edge (moved off the map column's
            // footer row so the disc gets the full card height). HF-only —
            // the headings are big-DX HF targets, so the VHF dial (and the
            // bare Station-page compass) never shows the rail.
            if (showPresets && (rotator?.showPresets ?? false))
              Positioned(
                right: 4,
                // Clears the stacked +/- stepper. The stepper buttons come
                // out 48 dp high despite the `minimumSize` override (the
                // theme's tap-target padding wins), so the stack is
                // 4 + 2×48 + 2 ≈ 102 tall — measured in widget tests.
                bottom: 108,
                child: const RotatorPresetsRail(),
              ),
          ],
        );
      },
    );
  }

  // Right-gutter geometry: the right half of the disc, vertical band from the
  // disc-right edge inward across the full disc height. Only drags whose first
  // hit-test lands here are eligible to drive zoom — taps elsewhere still aim
  // the rotator, and the left half stays free for left-handed rotation too.
  static bool _isRightGutter(Offset local, double cx, double cy, double r) {
    final dx = local.dx - cx;
    final dy = local.dy - cy;
    if (dx <= 0) return false;
    if (dx * dx + dy * dy > r * r) return false;
    return true;
  }
}

/// Stacked +/- zoom buttons, bottom-right of the compass module. Mirrors
/// horstreporter's `azimuth-zoom-in` / `azimuth-zoom-out` controls
/// (`static/index.html:314-317`, wired in `app.js:1472-1488`): ±0.2 step,
/// clamped to [kCompassZoomMin, kCompassZoomMax], disabled at the bounds.
/// Vertical drag on the disc's right gutter remains available as a
/// power-user gesture — the buttons are the discoverable entry point.
class _ZoomStepper extends StatelessWidget {
  final double zoom;
  final ValueChanged<double> onZoomChanged;
  const _ZoomStepper({required this.zoom, required this.onZoomChanged});

  @override
  Widget build(BuildContext context) {
    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        _ZoomIconButton(
          icon: Icons.add,
          tooltip: 'Zoom in (${kCompassZoomMin.toStringAsFixed(1)}×–${kCompassZoomMax.toStringAsFixed(1)}×)',
          onPressed: canZoomIn(zoom)
              ? () => onZoomChanged(zoomStep(zoom, up: true))
              : null,
        ),
        const SizedBox(height: 2),
        _ZoomIconButton(
          icon: Icons.remove,
          tooltip: 'Zoom out (${kCompassZoomMin.toStringAsFixed(1)}×–${kCompassZoomMax.toStringAsFixed(1)}×)',
          onPressed: canZoomOut(zoom)
              ? () => onZoomChanged(zoomStep(zoom, up: false))
              : null,
        ),
      ],
    );
  }
}

class _ZoomIconButton extends StatelessWidget {
  final IconData icon;
  final String tooltip;
  final VoidCallback? onPressed;
  const _ZoomIconButton({required this.icon, required this.tooltip, required this.onPressed});

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: tooltip,
      child: ElevatedButton(
        onPressed: onPressed,
        // Slightly smaller than the global `iconActionButton` (48×48) so the
        // stacked stepper stays compact under the preset rail at the card's
        // bottom-right corner.
        style: AppTheme.iconActionButton().copyWith(
          minimumSize: const WidgetStatePropertyAll(Size(40, 31)),
        ),
        child: Icon(icon),
      ),
    );
  }
}

/// Chip rail on the *left* of the compass module showing only the bands
/// currently in use, ordered by canonical HF band (lowest freq first →
/// highest). Each chip uses the matching horstreporter band color (see
/// `AppTheme.bandColor` + `docs/conventions/band-mode-reference.md`). Fades
/// in when banded spots arrive, out when they leave.
class _BandLegend extends StatelessWidget {
  static const List<String> _bandOrder = [
    '160m', '80m', '60m', '40m', '30m', '20m', '17m', '15m', '12m', '10m', '6m',
  ];

  /// Canonical band filter — same logic the web tool uses for its grid-square
  /// dominant-band palette (just the band labels that exist in
  /// `AppTheme.bandColor`).
  static List<String> visibleBands(List<DxSpot> spots) {
    final set = <String>{};
    for (final s in spots) {
      if (s.band.isNotEmpty) set.add(s.band);
    }
    return _bandOrder.where(set.contains).toList(growable: false);
  }

  final List<String> visible;
  final bool vertical;
  const _BandLegend({required this.visible, this.vertical = false});

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

/// Zoom-level badge in the compass header. Shows the current AEQD zoom ×
/// multiplier (e.g. "1.5×") and renders in `AppTheme.txtMute` so it doesn't
/// compete with the azimuth / target indicator visually.
class _ZoomBadge extends StatelessWidget {
  final double zoom;
  const _ZoomBadge({required this.zoom});

  @override
  Widget build(BuildContext context) {
    final label = '${zoom.toStringAsFixed(2)}×';
    return Tooltip(
      message:
          'Drag up/down on the right side of the disc to zoom (${kCompassZoomMin.toStringAsFixed(1)}×–${kCompassZoomMax.toStringAsFixed(1)}×)',
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
        decoration: BoxDecoration(
          color: AppTheme.pane,
          border: Border.all(color: AppTheme.cardLineHi),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Text(
          label,
          style: AppTheme.mono(11, color: AppTheme.txtMute, weight: FontWeight.w700),
        ),
      ),
    );
  }
}

/// Azimuth / target / moving / offline chip in the top-right of the module.
/// Color comes from the caller (red when moving, muted when offline, accent
/// otherwise). The chip is pinned to the module border instead of living in
/// a header row so the disc gets the full card height.
class _AzimuthChip extends StatelessWidget {
  final List<String> parts;
  final Color color;
  const _AzimuthChip({required this.parts, required this.color});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(
        parts.join(' · '),
        style: AppTheme.mono(12, weight: FontWeight.w700, color: color),
      ),
    );
  }
}

/// Read-out parts for the selected-station chip: `CALL · az° · dist`, each
/// segment only when the bridge derived it. Distance rounds to whole km —
/// beam-assessment precision, not pileup precision. With `aimable` the
/// bearing segment carries the `→` target marker (the same convention as
/// the azimuth pill's target read-out) because a tap on the chip will turn
/// the rotor onto that bearing.
List<String> _selectedChipParts(SelectedSpot sel, int ageSeconds, {bool aimable = false}) {
  final stale = stalenessFor(ageSeconds) != SelectedStaleness.fresh;
  String? azText;
  if (sel.azimuth != null) {
    azText = aimable ? '→ ${sel.azimuth!.round()}°' : '${sel.azimuth!.round()}°';
  }
  String? distText;
  if (sel.distanceKm != null) distText = '${sel.distanceKm!.round()} km';
  return [
    sel.call,
    if (sel.band.isNotEmpty) sel.band,
    if (azText != null) azText,
    if (distText != null) distText,
    if (stale) 'old',
  ];
}

/// Bottom-left chip naming the keyed station with the beam answer. Amber —
/// the theme's "attention, not alarm" color — and grey when the selection
/// is stale. Mirror of [_AzimuthChip] so the two read-outs read as one
/// family. It is also the actuator: a tap aims the rotor at the keyed
/// station. [onTap] is null when that can't work (no bearing, rotor
/// offline) and the chip stays a plain read-out.
class _SelectedChip extends StatelessWidget {
  final List<String> parts;
  final bool stale;
  final VoidCallback? onTap;
  const _SelectedChip({required this.parts, required this.stale, this.onTap});

  @override
  Widget build(BuildContext context) {
    final color = stale ? AppTheme.txtMute : AppTheme.amber;
    return GestureDetector(
      onTap: onTap,
      behavior: HitTestBehavior.opaque,
      child: Container(
        // Control first, read-out second: one-tap contract means a real
        // touch target — [kSelectedChipHeight] tall with wide padding, not
        // the ~20 dp the 12 px text alone would make.
        constraints: const BoxConstraints(minHeight: kSelectedChipHeight),
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 8),
        alignment: Alignment.center,
        decoration: BoxDecoration(
          color: AppTheme.blend(color, 0.12),
          border: Border.all(color: color),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Text(
          parts.join(' · '),
          style: AppTheme.mono(12, weight: FontWeight.w700, color: color),
        ),
      ),
    );
  }
}

class _CompassPainter extends CustomPainter {
  final double az;
  final String direction;
  final double targetAz;

  /// False on pages without a rotator: skip the beam fan, boom and target
  /// ray — the dial is a plain azimuthal DX map there.
  final bool showBeam;
  final List<GridSquare> gridSquares;
  final double? centerLat;
  final double? centerLng;
  final List<List<LatLng>>? worldRings;
  final double zoom;
  final WorldLayerCache world;
  final SelectedSpot? selected;
  final int selectedAgeSeconds;

  _CompassPainter({
    required this.az,
    required this.direction,
    required this.targetAz,
    this.showBeam = true,
    this.gridSquares = const [],
    this.centerLat,
    this.centerLng,
    this.worldRings,
    this.zoom = 1.0,
    required this.world,
    this.selected,
    this.selectedAgeSeconds = 0,
  });

  @override
  void paint(Canvas canvas, Size size) {
    final cx = size.width / 2;
    final cy = size.height / 2;
    final r = math.min(cx, cy) - kDiscInset;

    canvas.drawCircle(Offset(cx, cy), r, Paint()..color = AppTheme.card);
    canvas.drawCircle(
      Offset(cx, cy),
      r,
      Paint()
        ..color = AppTheme.cardLineHi
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1,
    );

    // Land masses, AEQD-projected from Natural Earth 50m: filled with the theme
    // land color (one step above the disc background for contrast) plus the
    // coastline stroke on top. Drawn before the tick marks so the ticks read
    // above the land. The bitmap is cached on `world` and reused across frames
    // while (cx, cy, r, zoom, center) don't change, so a zoom-drag re-blits the
    // same rasterized layer.
    _drawWorld(canvas, cx, cy, r, size);

    final tickPaint = Paint()
      ..color = AppTheme.txtFaint
      ..strokeWidth = 1;
    for (int a = 0; a < 360; a += 30) {
      final major = a % 90 == 0;
      final p1 = _pt(cx, cy, a.toDouble(), r);
      final p2 = _pt(cx, cy, a.toDouble(), major ? r - 12.0 : r - 6.0);
      canvas.drawLine(p1, p2, tickPaint..strokeWidth = major ? 2 : 1);
    }

    _label(canvas, 'N', cx, cy, 0.0, r - 20, AppTheme.accent, 14, FontWeight.w700);
    _label(canvas, 'E', cx, cy, 90.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);
    _label(canvas, 'S', cx, cy, 180.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);
    _label(canvas, 'W', cx, cy, 270.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);

    final beams = showBeam ? _beams() : const <_Beam>[];
    for (final b in beams) {
      final path = Path();
      path.moveTo(cx, cy);
      final a0 = b.ang - b.half;
      final a1 = b.ang + b.half;
      path.arcTo(
        Rect.fromCircle(center: Offset(cx, cy), radius: r - 4),
        (a0 - 90) * math.pi / 180,
        (a1 - a0) * math.pi / 180,
        false,
      );
      path.close();
      canvas.drawPath(
        path,
        Paint()
          ..color = AppTheme.blend(b.color, b.glow ? 0.55 : 0.40)
          ..style = PaintingStyle.fill,
      );
    }

    // The pointing indicator: one line + arrowhead, always on the boom
    // azimuth — where the antenna is pointed. Radiation is what the cones
    // show; they never carry lines or arrows of their own.
    if (showBeam) {
      final p1 = _pt(cx, cy, az, 28);
      final p2 = _pt(cx, cy, az, r - 14);
      canvas.drawLine(p1, p2, Paint()..color = AppTheme.accent..strokeWidth = 3.5..strokeCap = StrokeCap.round);
      _drawArrow(canvas, cx, cy, az, r - 10, AppTheme.accent);
    }

    _drawGridSquares(canvas, cx, cy, r);

    if (showBeam) {
      final boomStart = _pt(cx, cy, az, 16);
      final boomEnd = _pt(cx, cy, az, r - 2);
      canvas.drawLine(boomStart, boomEnd, Paint()..color = AppTheme.txt..strokeWidth = 2..strokeCap = StrokeCap.round);

      final targetDiff = (targetAz - az).abs();
      if (targetDiff > 5.0) {
        final targetStart = _pt(cx, cy, targetAz, 16);
        final targetEnd = _pt(cx, cy, targetAz, r - 6);
        canvas.drawLine(
          targetStart,
          targetEnd,
          Paint()
            ..color = AppTheme.blend(AppTheme.accent, 0.55)
            ..strokeWidth = 1.5
            ..strokeCap = StrokeCap.round,
        );
      }
    }

    // The QTH dot stays even without a rotator — it marks the station on
    // the bare DX map.
    canvas.drawCircle(Offset(cx, cy), 5, Paint()..color = AppTheme.accent);

    _drawSelected(canvas, cx, cy, r);
  }

  /// The station the operator keyed in the logger: a pin at its AEQD
  /// position when the bridge resolved coordinates, else a bearing ray at
  /// the logger-reported azimuth; callsign label next to either. Dimmed
  /// when stale (the operator probably moved on). Drawn last, above the
  /// beam fans and the boom, so it never hides under them.
  void _drawSelected(Canvas canvas, double cx, double cy, double r) {
    final sel = selected;
    if (sel == null) return;

    final stale = stalenessFor(selectedAgeSeconds) == SelectedStaleness.stale;
    final alpha = stale ? 0.45 : 1.0;
    final color = AppTheme.amber.withValues(alpha: alpha);
    final scale = r * zoom / math.pi;

    if (sel.hasPosition && centerLat != null && centerLng != null) {
      final aeqd = Aeqd(centerLat!, centerLng!);
      final n = aeqd.normalized(sel.lat!, sel.lng!);
      if (n != null) {
        final px = cx + n.x * scale;
        final py = cy - n.y * scale;
        // Pin: amber dot inside an open ring — distinct from the anonymous
        // band-colored spot squares and from the solid QTH dot.
        canvas.drawCircle(Offset(px, py), 6, Paint()..color = color);
        canvas.drawCircle(
          Offset(px, py),
          10,
          Paint()
            ..color = color
            ..style = PaintingStyle.stroke
            ..strokeWidth = 2,
        );
        _label(canvas, sel.call, px, py, 0, -16, color, 12, FontWeight.w700);
        return;
      }
      // Coordinates exist but sit past the projection horizon (near-antipode
      // clip): fall through to the azimuth ray so the bearing answer — the
      // whole point — still shows.
    }

    if (sel.hasBearing) {
      // Ray from center to the rim at the reported bearing; the label rides
      // just under the rim tick so it stays readable at any zoom.
      final start = _pt(cx, cy, sel.azimuth!, 16);
      final end = _pt(cx, cy, sel.azimuth!, r - 2);
      canvas.drawLine(
        start,
        end,
        Paint()
          ..color = color
          ..strokeWidth = 2.5
          ..strokeCap = StrokeCap.round,
      );
      _label(canvas, sel.call, cx, cy, sel.azimuth!, r - 30, color, 12, FontWeight.w700);
    }
  }

  /// Radiation cones only — where the elements radiate per direction mode.
  /// The pointing line + arrowhead is drawn separately at the boom azimuth.
  List<_Beam> _beams() {
    const half = {'forward': 30.0, 'reverse': 30.0, 'bidirectional': 45.0};
    return direction == 'forward'
        ? [_Beam(az, half['forward']!, AppTheme.accent, false)]
        : direction == 'reverse'
            ? [_Beam((az + 180) % 360, half['reverse']!, AppTheme.amber, true)]
            : [
                _Beam(az, half['bidirectional']!, AppTheme.accent, false),
                _Beam((az + 180) % 360, half['bidirectional']!, AppTheme.amber, true),
              ];
  }

  /// Fill + stroke the Natural Earth 50m land masses (bundled as
  /// `assets/geo/world.geojson`, AEQD-projected via the same `Aeqd` the spots
  /// use). No-op without a projection center or until the loader has produced
  /// rings. The ~100k-vertex projection is rasterized into a `ui.Picture` on the
  /// [WorldLayerCache] and blitted here — a zoom drag reuses the same bitmap
  /// while (cx, cy, r, zoom, center) don't change, dropping per-frame cost
  /// from ~6–10 ms to a single GPU blit. Polygons that wrap past the rim are
  /// clipped to the disc — same behavior as horstreporter's azimuthal canvas.
  void _drawWorld(Canvas canvas, double cx, double cy, double r, Size size) {
    final lat0 = centerLat;
    final lon0 = centerLng;
    final rings = worldRings;
    if (lat0 == null || lon0 == null || rings == null || rings.isEmpty) return;
    // The cache blits over the full widget rect; the disc clip is baked into
    // the rasterized picture itself.
    world.draw(
      canvas,
      Offset.zero & size,
      cx,
      cy,
      r,
      zoom,
      lat0,
      lon0,
      rings,
      AppTheme.land,
      AppTheme.cardLineHi,
    );
  }

  /// AEQD-project each Maidenhead 4-char grid square's SW / NE corners and fill
  /// the resulting quad with the square's dominant-band color (see
  /// `AppTheme.bandColor` + `docs/conventions/band-mode-reference.md`). No
  /// halo, no per-spot label — the square itself is the read-out. Opacity
  /// follows horstreporter's `gridSnrOpacity` ramp via `topQuartileMean` so
  /// active squares stand out without dominating the disc.
  ///
  /// All in-viewport squares are drawn — even ones whose AEQD-projected size
  /// is only a pixel or two at low zoom. horstreporter's
  /// `azimuth-runtime.js:1753-1784` `drawSpots` does the same: every grid
  /// square with valid corners renders, regardless of pixel size. The only
  /// skips are (a) corners off-disc (antipode wraparound) and (b) centroid
  /// off-disc (the cell wraps around to the back side). Without this, squares
  /// "vanish" at low zoom even though the data says they should be visible —
  /// a regression vs the web tool the user flagged live.
  void _drawGridSquares(Canvas canvas, double cx, double cy, double r) {
    final lat0 = centerLat;
    final lon0 = centerLng;
    if (lat0 == null || lon0 == null || gridSquares.isEmpty) return;

    final aeqd = Aeqd(lat0, lon0);
    final scale = r * zoom / math.pi;
    final discR2 = r * r;

    final stroke = Paint()
      ..color = AppTheme.cardLineHi
      ..style = PaintingStyle.stroke
      ..strokeWidth = 0.6
      ..strokeJoin = StrokeJoin.bevel
      ..strokeCap = StrokeCap.square;
    final fill = Paint()..style = PaintingStyle.fill;

    for (final sq in gridSquares) {
      final bounds = locatorToBounds(sq.locator);
      if (bounds == null) continue;

      // Project the four corners in (sw, se, ne, nw) order. NW uses the
      // NORTH lat + WEST lng (not ne.lat + ne.lng, which would collapse NW
      // onto NE and produce a triangle, not a quad).
      final sw = aeqd.normalized(bounds.sw.lat, bounds.sw.lng);
      final se = aeqd.normalized(bounds.sw.lat, bounds.ne.lng);
      final ne = aeqd.normalized(bounds.ne.lat, bounds.ne.lng);
      final nw = aeqd.normalized(bounds.ne.lat, bounds.sw.lng);
      // Skip if any corner is on the other side of the rim (antimeridian wrap
      // for a 2° wide square can give three nulls with one valid).
      if (sw == null || se == null || ne == null || nw == null) continue;

      final pts = <Offset>[
        Offset(cx + sw.x * scale, cy - sw.y * scale),
        Offset(cx + se.x * scale, cy - se.y * scale),
        Offset(cx + ne.x * scale, cy - ne.y * scale),
        Offset(cx + nw.x * scale, cy - nw.y * scale),
      ];
      // Drop squares whose centroid is fully off-disc (their corner pixels are
      // inside the disc bounds but the bulk of the square wraps around the
      // antipode — common for low zoom on distant stations). Every other
      // square with valid corners draws at its projected size, no matter how
      // small.
      final cdx = (pts[0].dx + pts[2].dx) / 2;
      final cdy = (pts[0].dy + pts[2].dy) / 2;
      if ((cdx - cx) * (cdx - cx) + (cdy - cy) * (cdy - cy) > discR2) continue;

      final color = AppTheme.bandColor(sq.dominantBand);
      final opacity = AppTheme.gridSnrOpacity(sq.score);

      fill.color = color.withValues(alpha: opacity);
      final path = Path()
        ..moveTo(pts[0].dx, pts[0].dy)
        ..lineTo(pts[1].dx, pts[1].dy)
        ..lineTo(pts[2].dx, pts[2].dy)
        ..lineTo(pts[3].dx, pts[3].dy)
        ..close();
      canvas.drawPath(path, fill);
      canvas.drawPath(path, stroke);
    }
  }

  Offset _pt(double cx, double cy, double angle, double radius) {
    final rad = (angle - 90) * math.pi / 180;
    return Offset(cx + radius * math.cos(rad), cy + radius * math.sin(rad));
  }

  void _label(Canvas c, String text, double cx, double cy, double angle, double r, Color color, double size, FontWeight w) {
    final p = _pt(cx, cy, angle, r);
    final tp = TextPainter(
      text: TextSpan(text: text, style: AppTheme.mono(size, color: color, weight: w)),
      textDirection: TextDirection.ltr,
      textAlign: TextAlign.center,
    );
    tp.layout();
    tp.paint(c, Offset(p.dx - tp.width / 2.0, p.dy - tp.height / 2.0));
  }

  void _drawArrow(Canvas c, double cx, double cy, double angle, double r, Color color) {
    final tip = _pt(cx, cy, angle, r);
    final path = Path();
    path.moveTo(tip.dx, tip.dy);
    final left = _pt(tip.dx, tip.dy, angle - 135, 11);
    final right = _pt(tip.dx, tip.dy, angle + 135, 11);
    path.lineTo(left.dx, left.dy);
    path.lineTo(right.dx, right.dy);
    path.close();
    c.drawPath(path, Paint()..color = color);
  }

  @override
  bool shouldRepaint(covariant _CompassPainter old) =>
      old.zoom != zoom ||
      old.az != az ||
      old.targetAz != targetAz ||
      old.direction != direction ||
      old.centerLat != centerLat ||
      old.centerLng != centerLng ||
      old.gridSquares.length != gridSquares.length ||
      !identical(old.gridSquares, gridSquares) ||
      (old.worldRings?.length ?? 0) != (worldRings?.length ?? 0) ||
      // Selected-station marker: repaint on identity change (new call/source)
      // and on half-minute age buckets (the dim-out), not per second.
      _selectedKey(old.selected, old.selectedAgeSeconds) != _selectedKey(selected, selectedAgeSeconds);

  String _selectedKey(SelectedSpot? sel, int ageSeconds) {
    if (sel == null) return '';
    return '${sel.call}|${sel.source}|${sel.tsMs}|${ageSeconds ~/ 30}';
  }
}

class _Beam {
  final double ang;
  final double half;
  final Color color;
  final bool glow;
  _Beam(this.ang, this.half, this.color, this.glow);
}
