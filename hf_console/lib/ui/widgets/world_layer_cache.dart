// world_layer_cache.dart — AEQD-projected world-landmass raster cache.
//
// The world layer is the single most expensive thing the compass painter does:
// ~1.6k rings / ~100k vertices × AEQD trig = tens of ms per rebuild, which
// would cap zoom-drag well below 60 fps if paid per frame. Mirrors
// horstreporter's `drawWorldCached` (azimuth-runtime.js:846-892), which caches
// an `OffscreenCanvas` of the coastline layer and blits it during the
// per-frame draw. Our Flutter analogue is a `ui.Picture` recorded once per
// (cx, cy, r, zoom, centerLat, centerLng, ring-count, colors) and reused as
// long as none of those change. Cache hit = single GPU blit (~0.2 ms);
// zoom-drag then costs only the grid-square + chrome layers.
//
// One cache instance lives on `_CompassPanelState` so it survives across
// rebuilds. Dispose with the state.

import 'dart:ui' as ui;
import 'dart:math' as math;
import 'package:flutter/material.dart';
import '../../dxspot/projection.dart';
import '../../dxspot/ring_subpaths.dart';

class WorldLayerCache {
  String? _key;
  ui.Picture? _picture;
  ui.Image? _image;
  int _width = 0;
  int _height = 0;

  String _makeKey(
    double cx,
    double cy,
    double r,
    double zoom,
    double? centerLat,
    double? centerLng,
    int ringCount,
    Color fillColor,
    Color strokeColor,
  ) =>
      'w=${cx.toStringAsFixed(1)},${cy.toStringAsFixed(1)}|'
      'r=${r.toStringAsFixed(1)}|'
      'z=${zoom.toStringAsFixed(2)}|'
      'c=${(centerLat ?? 0).toStringAsFixed(2)},${(centerLng ?? 0).toStringAsFixed(2)}|'
      'n=$ringCount|'
      'f=${fillColor.toARGB32()}|'
      's=${strokeColor.toARGB32()}';

  /// Raster the world landmass layer to the cache (when the key changes) and
  /// blit it onto [canvas] covering the whole widget rect. Land polygons are
  /// filled with [fillColor] (even-odd, so GeoJSON hole rings — Caspian,
  /// enclaves — stay unfilled) and the coastlines are stroked over with
  /// [strokeColor]. Returns `true` if the cache was rebuilt this call. No-op
  /// (returns false) when there's no center or rings to render.
  bool draw(
    Canvas canvas,
    Rect bounds,
    double cx,
    double cy,
    double r,
    double zoom,
    double? centerLat,
    double? centerLng,
    List<List<LatLng>>? rings,
    Color fillColor,
    Color strokeColor,
  ) {
    if (centerLat == null || centerLng == null || rings == null || rings.isEmpty) {
      // Without data, drop any stale cache.
      _key = null;
      return false;
    }
    final key = _makeKey(cx, cy, r, zoom, centerLat, centerLng, rings.length, fillColor, strokeColor);
    if (_key == key && _image != null) {
      paintImage(
        canvas: canvas,
        rect: bounds,
        image: _image!,
        filterQuality: FilterQuality.medium,
      );
      return false;
    }

    // Rebuild: rasterize the world into an offscreen Picture/Image.
    final w = bounds.width.ceil();
    final h = bounds.height.ceil();
    if (w <= 0 || h <= 0) return false;
    final recorder = ui.PictureRecorder();
    final c = Canvas(recorder, bounds);
    final clipPath = Path()..addOval(Rect.fromCircle(center: Offset(cx, cy), radius: r));
    c.save();
    c.clipPath(clipPath);

    // Project every ring into a single path. Even-odd fill makes hole rings
    // subtract from their enclosing outer ring. AEQD has no wrap seam; the
    // only discontinuity is the near-antipode null cap (land within 0.02 rad
    // of the antipode), which breaks a ring into subpaths there — see
    // ring_subpaths.dart. Latent caveat for exotic locators: each broken
    // subpath implicitly closes with a straight chord that can cut across the
    // disc interior (not along the rim), so a QTH whose antipode lies inside
    // or near landmass (New Zealand, Brazil, China) gets spurious filled
    // wedges near the antipode sector. For the Mühle QTH (antipode in the
    // South Pacific, ~700 km from any coastline) no ring ever reaches the cap
    // and the fill is exact.
    final aeqd = Aeqd(centerLat, centerLng);
    final scale = (r - 6) * zoom / math.pi;
    final subpaths = projectRingSubpaths(
      rings,
      (lat, lng) {
        final n = aeqd.normalized(lat, lng);
        if (n == null) return null;
        return (x: cx + n.x * scale, y: cy - n.y * scale);
      },
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
        ..color = fillColor
        ..style = PaintingStyle.fill
        ..isAntiAlias = true,
    );
    c.drawPath(
      path,
      Paint()
        ..color = strokeColor
        ..style = PaintingStyle.stroke
        ..strokeWidth = 0.8
        ..strokeJoin = StrokeJoin.round
        ..strokeCap = StrokeCap.round,
    );
    c.restore();
    final pic = recorder.endRecording();
    final img = pic.toImageSync(w, h);

    // Release the previous frame's resources before swapping.
    _picture?.dispose();
    _image?.dispose();
    _picture = pic;
    _image = img;
    _width = w;
    _height = h;
    _key = key;

    paintImage(
      canvas: canvas,
      rect: bounds,
      image: img,
      filterQuality: FilterQuality.medium,
    );
    return true;
  }

  bool get hasCachedImage => _image != null;
  int get width => _width;
  int get height => _height;

  void dispose() {
    _picture?.dispose();
    _picture = null;
    _image?.dispose();
    _image = null;
    _key = null;
  }
}