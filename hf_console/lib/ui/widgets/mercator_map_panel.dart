// mercator_map_panel.dart — Web-Mercator DX spot map.
//
// A companion to CompassPanel: same underlying DxSpotService data, but projected
// with EPSG:3857 instead of AEQD. Mirrors horstreporter's Leaflet map view
// (`static/map.js`): pan/zoom canvas, country landmass fills + coastline outlines,
// grid-square fills by dominant band + SNR opacity, and FT8/FT4 spot dots.

import 'dart:async';
import 'dart:ui' as ui;
import 'dart:math' as math;

import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../dxspot/dxspot_service.dart';
import '../../dxspot/map_fit.dart';
import '../../dxspot/mercator_projection.dart';
import '../../dxspot/overlay_geometry.dart';
import '../../dxspot/places.dart';
import '../../dxspot/projection.dart';
import '../../dxspot/ring_subpaths.dart';
import '../../dxspot/world_geometry.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/aim.dart';
import '../../store/bus_store.dart';
import '../../store/sat_track.dart';
import 'band_legend.dart';
import '../../store/selected_spot.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'rotator_presets_bar.dart';

const double kMercatorZoomMin = 1.0;
const double kMercatorZoomMax = 12.0;
const double _kMercatorZoomDefault = 2.5;
const double _kMercatorZoomStep = 0.5;

class MercatorMapPanel extends StatefulWidget {
  /// Whether to overlay the direction-preset rail on the right edge, above
  /// the zoom row (tablet layout; phones keep the horizontal bar in their
  /// scroll column). Mirrors `CompassPanel.showPresets`.
  final bool showPresets;

  /// Which rotator the page's compass dial reads — the Mercator view draws
  /// no dial itself, but the preset rail is an HF-rotator affordance, so it
  /// only shows when this surface carries the preset rail.
  final RotatorSurface? rotator;

  /// Zoom the map opens with; `null` falls back to [_kMercatorZoomDefault].
  final double? initialZoom;

  /// Zoom range of this panel (the VHF module zooms in to town level).
  final double minZoom;
  final double maxZoom;

  /// Draw [rotator]'s beam, aim it by tap and show a STOP (the VHF/UHF
  /// module). Off for the HF Mercator view: the HF beam depends on the
  /// Ultrabeam direction mode, which only the compass models.
  final bool rotatorOverlay;

  const MercatorMapPanel({
    super.key,
    this.showPresets = true,
    this.rotator = hfRotator,
    this.initialZoom,
    this.minZoom = kMercatorZoomMin,
    this.maxZoom = kMercatorZoomMax,
    this.rotatorOverlay = false,
  });

  @override
  State<MercatorMapPanel> createState() => _MercatorMapPanelState();
}

class _MercatorMapPanelState extends State<MercatorMapPanel> {
  late double _zoom = widget.initialZoom ?? _kMercatorZoomDefault;
  // "Reset" returns to the page's opening zoom, not the global default —
  // the UHF page opens at 4× and reset should stay there.
  late final double _zoomDefault = widget.initialZoom ?? _kMercatorZoomDefault;
  double? _centerLat;
  double? _centerLng;
  // While a satellite is tracked the view frames the station AND the
  // satellite (map_fit.dart). The first pan / pinch / zoom press hands the
  // view to the operator (_autoFit off) until the next pass starts or RESET
  // is pressed.
  bool _autoFit = true;
  bool _wasTracking = false;
  String? _trackedName;
  double _scaleBaseZoom = 0;
  List<List<LatLng>>? _rings;
  List<Place> _places = Places.instance.loaded;
  // Aging tick for the selected-station marker (dim/hide as the keyed call
  // grows old) — a quiet band produces no other rebuilds, so keep a slow
  // one alive.
  Timer? _ageTick;

  @override
  void initState() {
    super.initState();
    _loadGeometry();
    _ageTick = Timer.periodic(const Duration(seconds: 30), (_) {
      if (mounted) setState(() {});
    });
  }

  @override
  void dispose() {
    _ageTick?.cancel();
    super.dispose();
  }

  Future<void> _loadGeometry() async {
    final rings = await WorldGeometry.instance.load();
    if (mounted) setState(() => _rings = rings);
    final places = await Places.instance.load();
    if (mounted) setState(() => _places = places);
  }

  void _setZoom(double z) {
    final clamped = z.clamp(widget.minZoom, widget.maxZoom);
    if (clamped == _zoom) return;
    setState(() => _zoom = clamped);
  }

  /// One pinch / drag step: pan by [focalDelta], then zoom to [zoom] keeping
  /// the geographic point under [focal] fixed (pinch zooms where the fingers are).
  void _gestureStep(Offset focal, Offset focalDelta, double zoom, Size size) {
    final lat = _centerLat;
    final lng = _centerLng;
    if (lat == null || lng == null) return;
    final z = zoom.clamp(widget.minZoom, widget.maxZoom).toDouble();
    var cLat = lat;
    var cLng = lng;

    MercatorProjection projAt(double clat, double clng, double zz) =>
        MercatorProjection(centerLat: clat, centerLng: clng, zoom: zz, width: size.width, height: size.height);

    final panned = projAt(cLat, cLng, _zoom).unproject(size.width / 2 - focalDelta.dx, size.height / 2 - focalDelta.dy);
    if (panned != null) {
      cLat = panned.lat;
      cLng = panned.lng;
    }
    if (z != _zoom) {
      final anchor = projAt(cLat, cLng, _zoom).unproject(focal.dx, focal.dy);
      if (anchor != null) {
        final q = projAt(cLat, cLng, z).project(anchor.lat, anchor.lng);
        if (q != null) {
          final moved = projAt(cLat, cLng, z).unproject(size.width / 2 + (q.x - focal.dx), size.height / 2 + (q.y - focal.dy));
          if (moved != null) {
            cLat = moved.lat;
            cLng = moved.lng;
          }
        }
      }
    }
    setState(() {
      _centerLat = cLat;
      _centerLng = cLng;
      _zoom = z;
    });
  }

  @override
  Widget build(BuildContext context) {
    final dx = context.watch<DxSpotService>();
    final store = context.watch<BusStore>();
    final qthLat = dx.centerLat;
    final qthLng = dx.centerLng;

    // The station the operator keyed in the shack logger (muehle/hf/spots,
    // published by logger-spot-bridge). Pin + callsign when the bridge
    // resolved coordinates; nothing here needs the azimuth.
    final selected = SelectedSpot.fromSelected(store.stateValue('muehle/hf/spots', 'selected'));
    final nowMs = DateTime.now().millisecondsSinceEpoch;
    final selectedAge = selected?.ageSecondsAt(nowMs) ?? 0;
    final selectedLive = selected != null && stalenessFor(selectedAge) != SelectedStaleness.expired;
    final bands = visibleBands(dx.spots);

    // Keep the panel centred on the QTH until the user pans it.
    if (qthLat != null && qthLng != null && _centerLat == null) {
      _centerLat = qthLat;
      _centerLng = qthLng;
    }

    // The satellite OscarWatch is tracking (muehle/uhf/sat-track): VHF
    // module only — sat ops live on the UHF/CAM pages.
    final sat = widget.rotatorOverlay ? SatTrack.fromStore(store) : null;
    final tracking = sat != null && qthLat != null && qthLng != null;
    // A new pass (or a different satellite) re-frames; ending a pass with
    // auto-fit still on simply falls back to the view held underneath.
    if (tracking && (!_wasTracking || _trackedName != sat.name)) _autoFit = true;
    _wasTracking = tracking;
    _trackedName = tracking ? sat.name : null;

    final lat = _centerLat ?? qthLat ?? 0.0;
    final lng = _centerLng ?? qthLng ?? 0.0;

    // Rotator overlay (VHF module): beam, target line, tap-to-aim, STOP.
    // Gated exactly like the compass: the bridge's /status and our link.
    final surface = widget.rotatorOverlay ? widget.rotator : null;
    final rotatorOnline = surface != null && (store.slots[surface.stateSlot]?.isOnline ?? false) && store.linkUp;
    final az = surface == null ? null : store.stateValueAs<num>(surface.stateSlot, 'az')?.toDouble();
    final targetAz = surface == null ? null : store.stateValueAs<num>(surface.stateSlot, surface.targetKey)?.toDouble();
    final qth = (qthLat != null && qthLng != null) ? (lat: qthLat, lng: qthLng) : null;
    final beam = (surface != null && az != null && qth != null)
        ? (
            qth: qth,
            az: az,
            target: targetAz,
            half: surface.beamHalfWidthDeg,
            online: rotatorOnline,
            // The beam goes yellow while it follows a tracked satellite.
            color: sat != null ? AppTheme.amber : AppTheme.accent,
          )
        : null;
    final mqtt = context.read<MqttService>();

    return LayoutBuilder(
      builder: (context, constraints) {
        final size = Size(constraints.maxWidth, constraints.maxHeight);
        // Auto-fit frames the station and the satellite together; otherwise
        // the operator's own view.
        var viewLat = lat;
        var viewLng = lng;
        var viewZoom = _zoom;
        if (tracking && _autoFit) {
          final fit = fitPoints(
            (lat: qthLat, lng: qthLng),
            sat.position,
            width: size.width,
            height: size.height,
            minZoom: widget.minZoom,
            maxZoom: math.min(widget.maxZoom, _zoomDefault),
          );
          viewLat = fit.lat;
          viewLng = fit.lng;
          viewZoom = fit.zoom;
        }
        // Any hands-on zoom or pan takes the view over from auto-fit,
        // starting from exactly what is on screen.
        void takeOver() {
          if (!(tracking && _autoFit)) return;
          _autoFit = false;
          _centerLat = viewLat;
          _centerLng = viewLng;
          _zoom = viewZoom;
        }

        final proj = MercatorProjection(
          centerLat: viewLat,
          centerLng: viewLng,
          zoom: viewZoom,
          width: size.width,
          height: size.height,
        );
        return ClipRect(
          child: GestureDetector(
            // One recogniser for drag and pinch (pan and scale cannot be
            // combined): one finger pans, two fingers pan + zoom about the
            // fingers' centre.
            onScaleStart: (d) {
              takeOver();
              _scaleBaseZoom = _zoom;
            },
            onScaleUpdate: (d) {
              final z = d.scale == 1.0 ? _zoom : _scaleBaseZoom + math.log(d.scale) / math.ln2;
              _gestureStep(d.localFocalPoint, d.focalPointDelta, z, size);
            },
            // Tap aims the rotator at the great-circle bearing of the tapped
            // point — the Mercator twin of the compass disc's tap-to-aim.
            onTapUp: (rotatorOnline && qth != null)
                ? (d) {
                    final p = proj.unproject(d.localPosition.dx, d.localPosition.dy);
                    if (p == null) return;
                    final brg = initialBearing(qth, (lat: p.lat, lng: p.lng));
                    publishAim(mqtt, store, surface, brg.roundToDouble());
                  }
                : null,
            child: Listener(
              onPointerSignal: (event) {
                if (event is PointerScrollEvent) {
                  // Wheel up (negative scroll delta) zooms in; down zooms out.
                  final step = event.scrollDelta.dy < 0 ? _kMercatorZoomStep : -_kMercatorZoomStep;
                  takeOver();
                  _setZoom(_zoom + step);
                }
              },
              child: Stack(
                fit: StackFit.expand,
                children: [
                  // Own layer: chrome over the map (heading chip, rails) and
                  // anything else sharing the parent layer must not drag the
                  // whole map into every repaint.
                  RepaintBoundary(
                    child: CustomPaint(
                      size: size,
                      painter: _MercatorPainter(
                        projection: proj,
                        isDark: !AppTheme.isLight,
                        rings: _rings,
                        gridSquares: dx.gridSquares,
                        spots: dx.spots,
                        filter: dx.filter,
                        qthLat: qthLat,
                        qthLng: qthLng,
                        selected: selectedLive ? selected : null,
                        selectedAgeSeconds: selectedAge,
                        places: _places,
                        beam: beam,
                        sat: sat,
                      ),
                      child: SizedBox.expand(),
                    ),
                  ),
                  Positioned(
                    bottom: 12,
                    right: 12,
                    child: _ZoomControls(
                      zoom: viewZoom,
                      minZoom: widget.minZoom,
                      maxZoom: widget.maxZoom,
                      onZoomIn: () {
                        takeOver();
                        _setZoom(_zoom + _kMercatorZoomStep);
                      },
                      onZoomOut: () {
                        takeOver();
                        _setZoom(_zoom - _kMercatorZoomStep);
                      },
                      onReset: () {
                        // While tracking, RESET re-frames station + satellite.
                        if (tracking) setState(() => _autoFit = true);
                        _setZoom(_zoomDefault);
                        if (qthLat != null && qthLng != null) {
                          setState(() {
                            _centerLat = qthLat;
                            _centerLng = qthLng;
                          });
                        }
                      },
                    ),
                  ),
                  // Direction presets, stacked directly above the zoom row
                  // on the right map edge (tablet layout only, HF rotator
                  // only — the headings are HF big-DX targets).
                  if (widget.showPresets && surface == null && (widget.rotator?.showPresets ?? false))
                    Positioned(
                      // 16/52, not 12/48: the rail lost its 4 dp frame padding.
                      right: 16,
                      // Clears the zoom row: bottom 12 + ~32-high row + 4 gap.
                      bottom: 52,
                      child: const RotatorPresetsRail(),
                    ),
                  // VHF module: STOP halts every axis of the surface (az + el),
                  // same place as the HF STOP.
                  if (surface != null)
                    Positioned(
                      right: 16,
                      bottom: 52,
                      child: RotatorPresetsRail(rotator: surface),
                    ),
                  if (surface != null)
                    Positioned(
                      top: 8,
                      right: 8,
                      child: IgnorePointer(
                        child: _HeadingChip(az: az, target: targetAz, online: rotatorOnline, hasQth: qth != null),
                      ),
                    ),
                  // Band key, left rail — same geometry as the compass panel's.
                  // Makes the band contract visible: on the UHF page the feed
                  // arrives pre-narrowed to 2m/70cm and the key shows exactly
                  // that.
                  if (bands.isNotEmpty)
                    Positioned(
                      left: 8,
                      top: 40,
                      bottom: 40,
                      child: IgnorePointer(
                        child: Align(
                          alignment: Alignment.centerLeft,
                          child: BandLegend(
                            visible: bands,
                            vertical: true,
                          ),
                        ),
                      ),
                    ),
                ],
              ),
            ),
          ),
        );
      },
    );
  }
}

class _ZoomControls extends StatelessWidget {
  final double zoom;
  final double minZoom;
  final double maxZoom;
  final VoidCallback onZoomIn;
  final VoidCallback onZoomOut;
  final VoidCallback onReset;

  const _ZoomControls({
    required this.zoom,
    required this.minZoom,
    required this.maxZoom,
    required this.onZoomIn,
    required this.onZoomOut,
    required this.onReset,
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card.withValues(alpha: 0.85),
        borderRadius: BorderRadius.circular(4),
        border: Border.all(color: AppTheme.cardLine),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          _ZoomButton(icon: Icons.remove, onTap: onZoomOut, enabled: zoom > minZoom + 1e-9),
          _ZoomButton(icon: Icons.my_location, onTap: onReset),
          _ZoomButton(icon: Icons.add, onTap: onZoomIn, enabled: zoom < maxZoom - 1e-9),
        ],
      ),
    );
  }
}

class _ZoomButton extends StatelessWidget {
  final IconData icon;
  final VoidCallback onTap;
  final bool enabled;

  const _ZoomButton({
    required this.icon,
    required this.onTap,
    this.enabled = true,
  });

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      onTap: enabled ? onTap : null,
      child: Container(
        padding: const EdgeInsets.all(6),
        child: Icon(
          icon,
          size: 18,
          color: enabled ? AppTheme.txt : AppTheme.txtMute,
        ),
      ),
    );
  }
}

/// Rotator read-out on the VHF map: where the array points, where it is
/// going, or why tapping will not aim it.
class _HeadingChip extends StatelessWidget {
  final double? az;
  final double? target;
  final bool online;
  final bool hasQth;

  const _HeadingChip({required this.az, required this.target, required this.online, required this.hasQth});

  @override
  Widget build(BuildContext context) {
    final String text;
    final Color color;
    if (!online) {
      text = 'AZ ROTATOR OFFLINE';
      color = AppTheme.red;
    } else if (!hasQth) {
      text = 'SET STATION LOCATOR TO AIM';
      color = AppTheme.amber;
    } else {
      final a = az == null ? '---' : '${az!.round()}°';
      final t = target;
      final moving = az != null && t != null && _angleDiff(t, az!) > 2;
      text = moving ? 'AZ $a → ${t.round()}°' : 'AZ $a · TAP TO AIM';
      color = AppTheme.accent;
    }
    return Container(
      key: const ValueKey('vhf-heading-chip'),
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
      decoration: BoxDecoration(
        color: AppTheme.card.withValues(alpha: 0.85),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(text, style: AppTheme.mono(11, color: color, weight: FontWeight.w700)),
    );
  }
}

double _angleDiff(double a, double b) {
  final d = (a - b).abs() % 360;
  return d > 180 ? 360 - d : d;
}

/// Grid-square fill strength by Mercator zoom: full up to zoom 4 (squares
/// are small there), falling linearly to 30 % at zoom 8 and closer.
double gridZoomFade(double zoom) {
  if (zoom <= 4) return 1.0;
  if (zoom >= 8) return 0.3;
  return 1.0 - (zoom - 4) / 4 * 0.7;
}

/// Rotator beam for the painter: QTH, pointing and commanded azimuth.
typedef MercatorBeam = ({LatLng qth, double az, double? target, double half, bool online, Color color});

/// Test hook: paint calls of the Mercator map painter.
@visibleForTesting
class MercatorPainterDebug {
  static int get paintCount => _MercatorPainter.debugPaintCount;
  static MercatorProjection? get lastProjection => _MercatorPainter.debugLastProjection;
  static MercatorBeam? get lastBeam => _MercatorPainter.debugLastBeam;
  static set paintCount(int v) => _MercatorPainter.debugPaintCount = v;

  /// The map painter with only the layers the overlay tests exercise.
  static CustomPainter overlayPainter({
    required MercatorProjection projection,
    required LatLng qth,
    MercatorBeam? beam,
    SatTrack? sat,
    List<List<LatLng>>? rings,
  }) =>
      _MercatorPainter(
        projection: projection,
        isDark: true,
        rings: rings,
        gridSquares: const [],
        spots: const [],
        filter: DxSpotFilter(),
        qthLat: qth.lat,
        qthLng: qth.lng,
        beam: beam,
        sat: sat,
      );
}

class _MercatorPainter extends CustomPainter {
  final MercatorProjection projection;
  final bool isDark;
  final List<List<LatLng>>? rings;
  final List<GridSquare> gridSquares;
  final List<DxSpot> spots;
  final DxSpotFilter filter;
  final double? qthLat;
  final double? qthLng;
  final SelectedSpot? selected;
  final int selectedAgeSeconds;
  final List<Place> places;
  final MercatorBeam? beam;
  final SatTrack? sat;

  _MercatorPainter({
    required this.projection,
    required this.isDark,
    required this.rings,
    required this.gridSquares,
    required this.spots,
    required this.filter,
    required this.qthLat,
    required this.qthLng,
    this.selected,
    this.selectedAgeSeconds = 0,
    this.places = const [],
    this.beam,
    this.sat,
  });

  /// Paint calls, for the repaint-discipline test.
  static int debugPaintCount = 0;
  static MercatorProjection? debugLastProjection;
  static MercatorBeam? debugLastBeam;

  @override
  void paint(Canvas canvas, Size size) {
    debugPaintCount++;
    debugLastProjection = projection;
    debugLastBeam = beam;
    // 1. Background
    canvas.drawRect(
      Offset.zero & size,
      Paint()..color = AppTheme.page,
    );

    // 2. World landmass fill + coastline outlines
    if (rings != null && rings!.isNotEmpty) {
      _drawWorld(canvas, size, rings!);
    }

    // 3. Grid-square fills by dominant band + SNR opacity
    for (final sq in gridSquares) {
      _drawGridSquare(canvas, sq);
    }

    // 3b. Place names (towns to aim at when zoomed in)
    _drawPlaces(canvas, size);

    // 3c. Rotator beam, boom and target line (VHF module)
    final b = beam;
    if (b != null) _drawBeam(canvas, size, b);

    // 4. Spot dots
    for (final spot in spots) {
      final p = projection.project(spot.lat, spot.lng);
      if (p == null) continue;
      final color = spot.band.isNotEmpty
          ? AppTheme.bandColor(spot.band)
          : AppTheme.spotColor(spot.sourceType);
      canvas.drawCircle(
        Offset(p.x, p.y),
        2.5,
        Paint()..color = color,
      );
      // White outline for contrast on dark fills.
      canvas.drawCircle(
        Offset(p.x, p.y),
        2.5,
        Paint()
          ..color = Colors.white.withValues(alpha: 0.6)
          ..style = PaintingStyle.stroke
          ..strokeWidth = 0.6,
      );
    }

    // 5. QTH marker
    if (qthLat != null && qthLng != null) {
      final p = projection.project(qthLat!, qthLng!);
      if (p != null) {
        canvas.drawCircle(
          Offset(p.x, p.y),
          5.0,
          Paint()..color = AppTheme.accent,
        );
        canvas.drawCircle(
          Offset(p.x, p.y),
          5.0,
          Paint()
            ..color = AppTheme.page
            ..style = PaintingStyle.stroke
            ..strokeWidth = 1.2,
        );
      }
    }

    // 6. Selected station (the operator-keyed call from the shack logger):
    // amber pin + callsign label, dimmed when stale. Only drawable with
    // coordinates — the azimuth-only case is the compass ray's job.
    final sel = selected;
    if (sel != null && sel.hasPosition) {
      final p = projection.project(sel.lat!, sel.lng!);
      if (p != null) {
        final stale = stalenessFor(selectedAgeSeconds) == SelectedStaleness.stale;
        final color = AppTheme.amber.withValues(alpha: stale ? 0.45 : 1.0);
        canvas.drawCircle(Offset(p.x, p.y), 5, Paint()..color = color);
        canvas.drawCircle(
          Offset(p.x, p.y),
          9,
          Paint()
            ..color = color
            ..style = PaintingStyle.stroke
            ..strokeWidth = 2,
        );
        _drawLabel(canvas, sel.call, p.x + 12, p.y - 12, color);
      }
    }

    // 7. Tracked satellite (VHF module): footprint, the great circle from
    // the station to the sub-satellite point, and the pin with its label.
    final st = sat;
    if (st != null) _drawSat(canvas, size, st);
  }

  /// The tracked satellite. Green (amber is the keyed station, cyan the
  /// rotator); everything dimmed while it is below the horizon. The station
  /// line matters most when zoomed in: the satellite itself is usually off
  /// screen, the line shows where it is relative to the rotator beam.
  void _drawSat(Canvas canvas, Size size, SatTrack st) {
    final color = AppTheme.green.withValues(alpha: st.inRange ? 1.0 : 0.5);

    // Footprint: the ground circle the satellite is above the horizon for.
    // Both curves are clipped to the world rectangle and repeated across the
    // wrap seam (see overlay_geometry.dart).
    canvas.save();
    canvas.clipRect(_worldRect);
    final radius = st.footprintRadiusKm;
    if (radius != null) {
      final path = Path();
      _addLine(path, projectLine(projection, circleSamples(st.position, radius)));
      canvas.drawPath(
        path,
        Paint()
          ..color = color.withValues(alpha: color.a * 0.6)
          ..style = PaintingStyle.stroke
          ..strokeWidth = 1.5,
      );
    }

    // Station → sub-satellite point along the great circle.
    final q = (qthLat != null && qthLng != null) ? (lat: qthLat!, lng: qthLng!) : null;
    ProjectedLine? stationLine;
    if (q != null) {
      stationLine = projectLine(
          projection, greatCircleSamples(q, initialBearing(q, st.position), distanceKm(q, st.position)));
      final path = Path();
      _addLine(path, stationLine);
      canvas.drawPath(
        path,
        Paint()
          ..color = color.withValues(alpha: color.a * 0.8)
          ..style = PaintingStyle.stroke
          ..strokeWidth = 1.5,
      );
    }
    canvas.restore();

    // Pin: a diamond, so it never reads as a spot dot or the keyed station.
    final view = (Offset.zero & size).deflate(8);
    // Beyond the Mercator limit (a polar pass) the bird is not on the map:
    // it gets the off-map label, not a pin squashed onto the edge.
    final p = MercatorProjection.latInBounds(st.lat) ? projection.project(st.lat, st.lng) : null;
    if (p == null || !view.contains(Offset(p.x, p.y))) {
      // Off screen (the usual case at the UHF page's town-level zoom): name
      // the satellite where its station line leaves the map.
      final edge = stationLine == null ? null : _lastVisible(stationLine, view);
      if (edge != null) {
        // Kept clear of the right-hand rail (zoom row, STOP/PARK, ~110 dp
        // wide) and the heading chip top-right.
        final w = _labelPainter(st.label, color).width + 8;
        final x = math.max(8.0, math.min(edge.dx, size.width - 120 - w));
        final y = edge.dy.clamp(48.0, size.height - 48.0);
        _drawLabel(canvas, st.label, x, y, color);
      }
      return;
    }
    final c = Offset(p.x, p.y);
    final diamond = Path()
      ..moveTo(c.dx, c.dy - 7)
      ..lineTo(c.dx + 7, c.dy)
      ..lineTo(c.dx, c.dy + 7)
      ..lineTo(c.dx - 7, c.dy)
      ..close();
    canvas.drawPath(diamond, Paint()..color = color);
    canvas.drawPath(
      diamond,
      Paint()
        ..color = AppTheme.page
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1.2,
    );
    _drawLabel(canvas, st.label, c.dx + 12, c.dy - 12, color);
  }

  /// Cities from the bundled Natural Earth layer, thinned by zoom (only
  /// important places when zoomed out) and by label overlap (most important
  /// first — the asset is sorted that way).
  void _drawPlaces(Canvas canvas, Size size) {
    if (places.isEmpty) return;
    final maxRank = Places.maxRankForZoom(projection.zoom);
    final taken = <Rect>[];
    final dot = Paint()..color = AppTheme.txtMute;
    for (final pl in places) {
      if (pl.rank > maxRank) continue;
      final p = projection.project(pl.pos.lat, pl.pos.lng);
      if (p == null || p.x < -40 || p.y < -20 || p.x > size.width + 40 || p.y > size.height + 20) continue;
      final tp = _placeLabel(pl.name);
      final rect = Rect.fromLTWH(p.x - 3, p.y - tp.height / 2, tp.width + 10, tp.height);
      if (taken.any((r) => r.overlaps(rect))) continue;
      taken.add(rect);
      canvas.drawCircle(Offset(p.x, p.y), 2.2, dot);
      tp.paint(canvas, Offset(p.x + 5, p.y - tp.height / 2));
    }
  }

  // Laid-out city labels, reused across repaints (a label only depends on
  // its name and the colour scheme). Reset when the scheme changes.
  static final Map<String, TextPainter> _labelCache = {};
  static AppColorScheme? _labelScheme;

  static TextPainter _placeLabel(String name) {
    if (_labelScheme != AppTheme.selected) {
      for (final tp in _labelCache.values) {
        tp.dispose();
      }
      _labelCache.clear();
      _labelScheme = AppTheme.selected;
    }
    return _labelCache.putIfAbsent(
      name,
      () => TextPainter(
        text: TextSpan(text: name, style: AppTheme.body(10, color: AppTheme.txtMute)),
        textDirection: TextDirection.ltr,
      )..layout(),
    );
  }

  /// Canvas rectangle the world occupies (the map is blank outside it);
  /// overlay geometry is clipped to it.
  Rect get _worldRect {
    final b = projection.worldBounds;
    return Rect.fromLTRB(b.left, b.top, b.right, b.bottom);
  }

  /// Adds [line] to [path] once per world shift (the copies past the seam).
  static void _addLine(Path path, ProjectedLine line) {
    if (line.isEmpty) return;
    for (final s in line.shifts) {
      path.moveTo(line.points.first.x + s, line.points.first.y);
      for (var i = 1; i < line.points.length; i++) {
        path.lineTo(line.points[i].x + s, line.points[i].y);
      }
    }
  }

  /// The last sample of [line] that is on screen and on the map, or null.
  Offset? _lastVisible(ProjectedLine line, Rect view) {
    final world = _worldRect;
    Offset? last;
    for (final p in line.points) {
      for (final s in line.shifts) {
        final o = Offset(p.x + s, p.y);
        if (view.contains(o) && world.contains(o)) last = o;
      }
    }
    return last;
  }

  /// Beam wedge along great circles from the QTH (az ± half) out to the
  /// farthest part of the viewport, the boom line on az and a faint target
  /// line while turning — the compass rules, drawn in Mercator. Zoomed out the
  /// rays run to the far side of the earth: across the antimeridian seam and
  /// over the poles, see overlay_geometry.dart.
  void _drawBeam(Canvas canvas, Size size, MercatorBeam b) {
    final reach = beamReachKm(projection, b.qth);

    canvas.save();
    canvas.clipRect(_worldRect);

    final wedge = Path();
    for (final quad in beamWedge(projection, b.qth, b.az, b.half, reach)) {
      for (final s in quad.shifts) {
        wedge.moveTo(quad.points[0].x + s, quad.points[0].y);
        for (var i = 1; i < 4; i++) {
          wedge.lineTo(quad.points[i].x + s, quad.points[i].y);
        }
        wedge.close();
      }
    }
    final alpha = b.online ? 0.30 : 0.12;
    canvas.drawPath(wedge, Paint()..color = AppTheme.blend(b.color, alpha));

    void line(double brg, Paint paint) {
      final path = Path();
      _addLine(path, beamRay(projection, b.qth, brg, reach));
      canvas.drawPath(path, paint);
    }

    final t = b.target;
    if (t != null && _angleDiff(t, b.az) > 5) {
      line(
          t,
          Paint()
            ..color = AppTheme.blend(b.color, 0.55)
            ..style = PaintingStyle.stroke
            ..strokeWidth = 1.5);
    }
    line(
        b.az,
        Paint()
          ..color = b.color
          ..style = PaintingStyle.stroke
          ..strokeWidth = 2.5
          ..strokeCap = StrokeCap.round);
    canvas.restore();
  }

  TextPainter _labelPainter(String text, Color color) => TextPainter(
        text: TextSpan(text: text, style: AppTheme.mono(11, weight: FontWeight.w700, color: color)),
        textDirection: TextDirection.ltr,
      )..layout();

  /// Callsign label on a small dark pill so it reads on any land fill.
  void _drawLabel(Canvas canvas, String text, double x, double y, Color color) {
    final tp = _labelPainter(text, color);
    final rect = Rect.fromLTWH(x, y - tp.height / 2, tp.width + 8, tp.height + 4);
    canvas.drawRRect(
      RRect.fromRectAndRadius(rect, const Radius.circular(3)),
      Paint()..color = AppTheme.page.withValues(alpha: 0.82),
    );
    tp.paint(canvas, Offset(x + 4, y - tp.height / 2 + 2));
  }

  void _drawGridSquare(Canvas canvas, GridSquare sq) {
    final bounds = locatorToBounds(sq.locator);
    if (bounds == null) return;
    final sw = projection.project(bounds.sw.lat, bounds.sw.lng);
    final se = projection.project(bounds.sw.lat, bounds.ne.lng);
    final ne = projection.project(bounds.ne.lat, bounds.ne.lng);
    final nw = projection.project(bounds.ne.lat, bounds.sw.lng);
    if (sw == null || se == null || ne == null || nw == null) return;

    final path = Path()
      ..moveTo(sw.x, sw.y)
      ..lineTo(se.x, se.y)
      ..lineTo(ne.x, ne.y)
      ..lineTo(nw.x, nw.y)
      ..close();

    final color = AppTheme.bandColor(sq.dominantBand);
    // Zoomed in, a 4-char square (2°×1°) covers much of the map; fade it so
    // coastlines, towns and the beam stay readable. The outline fades less,
    // keeping the square's extent visible.
    final fade = gridZoomFade(projection.zoom);
    final opacity = AppTheme.gridSnrOpacity(sq.score) * fade;
    canvas.drawPath(
      path,
      Paint()
        ..color = color.withValues(alpha: opacity)
        ..style = PaintingStyle.fill,
    );
    canvas.drawPath(
      path,
      Paint()
        ..color = color.withValues(alpha: math.min(1.0, AppTheme.gridSnrOpacity(sq.score) + 0.2) * math.max(fade, 0.5))
        ..style = PaintingStyle.stroke
        ..strokeWidth = 0.6,
    );
  }

  // Cached raster of the projected world layer. Rebuilt only when the projection
  // key changes, so pan/zoom drags are cheap.
  static String? _cacheKey;
  static ui.Picture? _cachePicture;
  static ui.Image? _cacheImage;

  void _drawWorld(Canvas canvas, Size size, List<List<LatLng>> rings) {
    final key = '${projection.centerLat.toStringAsFixed(3)}|'
        '${projection.centerLng.toStringAsFixed(3)}|'
        '${projection.zoom.toStringAsFixed(2)}|'
        '${size.width.toStringAsFixed(0)}|'
        '${size.height.toStringAsFixed(0)}|'
        '${isDark ? 'd' : 'l'}|'
        '${AppTheme.selected.name}';

    if (_cacheKey == key && _cacheImage != null) {
      paintImage(
        canvas: canvas,
        rect: Offset.zero & size,
        image: _cacheImage!,
        filterQuality: FilterQuality.low,
      );
      return;
    }

    final recorder = ui.PictureRecorder();
    final c = Canvas(recorder, Offset.zero & size);

    // Project every ring into a single path. Even-odd fill makes GeoJSON hole
    // rings subtract from their enclosing outer ring. Subpaths are CUT at the
    // projection's wrap seam — lng = centerLng ± 180°, moving with every pan
    // (NOT the raw ±180° dateline: Natural Earth rings are pre-cut there with
    // no raw Δlng jump). The seam resolver interpolates each crossing vertex
    // onto both canvas edges so every piece closes along the seam; a plain
    // unsplit segment is both stroked and used as a fill boundary, streaking
    // a land-colored band across the whole canvas. See ring_subpaths.dart.
    final subpaths = projectRingSubpaths(
      rings,
      (lat, lng) {
        final m = projection.project(lat, lng);
        if (m == null) return null;
        return (x: m.x, y: m.y);
      },
      seamCrossing: projection.seamCrossingBetween,
    );
    final path = Path()..fillType = PathFillType.evenOdd;
    for (final s in subpaths) {
      path.moveTo(s[0].x, s[0].y);
      for (int i = 1; i < s.length; i++) {
        path.lineTo(s[i].x, s[i].y);
      }
    }

    c.drawPath(
      path,
      Paint()
        ..color = AppTheme.land
        ..style = PaintingStyle.fill
        ..isAntiAlias = true,
    );
    c.drawPath(
      path,
      Paint()
        ..color = AppTheme.cardLineHi
        ..style = PaintingStyle.stroke
        ..strokeWidth = 0.8
        ..strokeJoin = StrokeJoin.round
        ..strokeCap = StrokeCap.round,
    );

    final pic = recorder.endRecording();
    final img = pic.toImageSync(size.width.ceil(), size.height.ceil());

    _cachePicture?.dispose();
    _cacheImage?.dispose();
    _cachePicture = pic;
    _cacheImage = img;
    _cacheKey = key;

    paintImage(
      canvas: canvas,
      rect: Offset.zero & size,
      image: _cacheImage!,
      filterQuality: FilterQuality.low,
    );
  }

  @override
  bool shouldRepaint(covariant _MercatorPainter oldDelegate) {
    return oldDelegate.projection != projection ||
        oldDelegate.isDark != isDark ||
        oldDelegate.rings != rings ||
        // DxSpotService publishes a new list on every change, so identity
        // is exact (length missed same-size updates once the projection
        // stopped forcing a repaint on every rebuild).
        !identical(oldDelegate.gridSquares, gridSquares) ||
        !identical(oldDelegate.spots, spots) ||
        oldDelegate.filter != filter ||
        oldDelegate.places.length != places.length ||
        oldDelegate.beam != beam ||
        oldDelegate.sat?.paintKey != sat?.paintKey ||
        // Selected-station marker: identity change (new call/source) or a
        // half-minute age bucket (the dim-out).
        _selectedKey(oldDelegate.selected, oldDelegate.selectedAgeSeconds) !=
            _selectedKey(selected, selectedAgeSeconds);
  }

  String _selectedKey(SelectedSpot? sel, int ageSeconds) {
    if (sel == null) return '';
    return '${sel.call}|${sel.source}|${sel.tsMs}|${ageSeconds ~/ 30}';
  }
}
