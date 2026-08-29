// mercator_map_panel.dart — Web-Mercator DX spot map.
//
// A companion to CompassPanel: same underlying DxSpotService data, but projected
// with EPSG:3857 instead of AEQD. Mirrors horstreporter's Leaflet map view
// (`static/map.js`): pan/zoom canvas, country landmass fills + coastline outlines,
// grid-square fills by dominant band + SNR opacity, and FT8/FT4 spot dots.

import 'dart:ui' as ui;
import 'dart:math' as math;

import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../dxspot/dxspot_service.dart';
import '../../dxspot/mercator_projection.dart';
import '../../dxspot/projection.dart';
import '../../dxspot/world_geometry.dart';
import '../theme.dart';

const double _kMercatorZoomMin = 1.0;
const double _kMercatorZoomMax = 12.0;
const double _kMercatorZoomDefault = 2.5;
const double _kMercatorZoomStep = 0.5;

class MercatorMapPanel extends StatefulWidget {
  const MercatorMapPanel({super.key});

  @override
  State<MercatorMapPanel> createState() => _MercatorMapPanelState();
}

class _MercatorMapPanelState extends State<MercatorMapPanel> {
  double _zoom = _kMercatorZoomDefault;
  double? _centerLat;
  double? _centerLng;
  List<List<LatLng>>? _rings;

  @override
  void initState() {
    super.initState();
    _loadGeometry();
  }

  Future<void> _loadGeometry() async {
    final rings = await WorldGeometry.instance.load();
    if (mounted) setState(() => _rings = rings);
  }

  void _setZoom(double z) {
    final clamped = z.clamp(_kMercatorZoomMin, _kMercatorZoomMax);
    if (clamped == _zoom) return;
    setState(() => _zoom = clamped);
  }

  void _panBy(double dx, double dy, Size size) {
    final lat = _centerLat;
    final lng = _centerLng;
    if (lat == null || lng == null) return;
    final proj = MercatorProjection(
      centerLat: lat,
      centerLng: lng,
      zoom: _zoom,
      width: size.width,
      height: size.height,
    );
    final next = proj.unproject(size.width / 2 + dx, size.height / 2 + dy);
    if (next == null) return;
    setState(() {
      _centerLat = next.lat;
      _centerLng = next.lng;
    });
  }

  @override
  Widget build(BuildContext context) {
    final dx = context.watch<DxSpotService>();
    final qthLat = dx.centerLat;
    final qthLng = dx.centerLng;

    // Keep the panel centred on the QTH until the user pans it.
    if (qthLat != null && qthLng != null && _centerLat == null) {
      _centerLat = qthLat;
      _centerLng = qthLng;
    }

    final lat = _centerLat ?? qthLat ?? 0.0;
    final lng = _centerLng ?? qthLng ?? 0.0;

    return LayoutBuilder(
      builder: (context, constraints) {
        final size = Size(constraints.maxWidth, constraints.maxHeight);
        final proj = MercatorProjection(
          centerLat: lat,
          centerLng: lng,
          zoom: _zoom,
          width: size.width,
          height: size.height,
        );
        return ClipRect(
          child: GestureDetector(
            onPanUpdate: (d) => _panBy(-d.delta.dx, -d.delta.dy, size),
            child: Listener(
              onPointerSignal: (event) {
                if (event is PointerScrollEvent) {
                  // Wheel up (negative scroll delta) zooms in; down zooms out.
                  final step = event.scrollDelta.dy < 0 ? _kMercatorZoomStep : -_kMercatorZoomStep;
                  _setZoom(_zoom + step);
                }
              },
              child: Stack(
                fit: StackFit.expand,
                children: [
                  CustomPaint(
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
                    ),
                    child: SizedBox.expand(),
                  ),
                  Positioned(
                    bottom: 12,
                    right: 12,
                    child: _ZoomControls(
                      zoom: _zoom,
                      onZoomIn: () => _setZoom(_zoom + _kMercatorZoomStep),
                      onZoomOut: () => _setZoom(_zoom - _kMercatorZoomStep),
                      onReset: () {
                        _setZoom(_kMercatorZoomDefault);
                        if (qthLat != null && qthLng != null) {
                          setState(() {
                            _centerLat = qthLat;
                            _centerLng = qthLng;
                          });
                        }
                      },
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
  final VoidCallback onZoomIn;
  final VoidCallback onZoomOut;
  final VoidCallback onReset;

  const _ZoomControls({
    required this.zoom,
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
          _ZoomButton(icon: Icons.remove, onTap: onZoomOut, enabled: zoom > _kMercatorZoomMin + 1e-9),
          _ZoomButton(icon: Icons.my_location, onTap: onReset),
          _ZoomButton(icon: Icons.add, onTap: onZoomIn, enabled: zoom < _kMercatorZoomMax - 1e-9),
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

class _MercatorPainter extends CustomPainter {
  final MercatorProjection projection;
  final bool isDark;
  final List<List<LatLng>>? rings;
  final List<GridSquare> gridSquares;
  final List<DxSpot> spots;
  final DxSpotFilter filter;
  final double? qthLat;
  final double? qthLng;

  _MercatorPainter({
    required this.projection,
    required this.isDark,
    required this.rings,
    required this.gridSquares,
    required this.spots,
    required this.filter,
    required this.qthLat,
    required this.qthLng,
  });

  @override
  void paint(Canvas canvas, Size size) {
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
    final opacity = AppTheme.gridSnrOpacity(sq.score);
    canvas.drawPath(
      path,
      Paint()
        ..color = color.withValues(alpha: opacity)
        ..style = PaintingStyle.fill,
    );
    canvas.drawPath(
      path,
      Paint()
        ..color = color.withValues(alpha: math.min(1.0, opacity + 0.2))
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
    // rings subtract from their enclosing outer ring. Natural Earth admin_0
    // polygons are pre-cut at the antimeridian, so a raw Δlng > 180° between
    // consecutive vertices marks the cut: split the subpath there so the
    // implicit close chords at the seam instead of streaking across the
    // canvas (the old stroke-only draw skipped those segments by length).
    final path = Path()..fillType = PathFillType.evenOdd;
    for (final ring in rings) {
      Offset? prev;
      double? prevLng;
      for (final p in ring) {
        final m = projection.project(p.lat, p.lng);
        if (m == null || (prevLng != null && (p.lng - prevLng).abs() > 180.0)) {
          // Out of Mercator domain, or dateline cut — break the subpath.
          prev = null;
          prevLng = p.lng;
          if (m == null) continue;
        } else {
          prevLng = p.lng;
        }
        final o = Offset(m.x, m.y);
        if (prev == null) {
          path.moveTo(o.dx, o.dy);
        } else {
          path.lineTo(o.dx, o.dy);
        }
        prev = o;
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
        oldDelegate.gridSquares.length != gridSquares.length ||
        oldDelegate.spots.length != spots.length ||
        oldDelegate.filter != filter;
  }
}
