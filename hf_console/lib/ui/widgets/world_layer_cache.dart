// world_layer_cache.dart — AEQD-projected world-coastline raster cache.
//
// The world layer is the single most expensive thing the compass painter does:
// ~30k rings × hundreds of vertices × AEQD trig = ~6–10 ms per frame, which
// caps the zoom-drag at ~10 fps. Mirrors horstreporter's `drawWorldCached`
// (azimuth-runtime.js:846-892), which caches an `OffscreenCanvas` of the
// coastline layer and blits it during the per-frame draw. Our Flutter
// analogue is a `ui.Picture` recorded once per (cx, cy, r, zoom, centerLat,
// centerLng, ring-count) and reused as long as none of those change. Cache
// hit = single GPU blit (~0.2 ms); zoom-drag then costs only the grid-square
// + chrome layers.
//
// One cache instance lives on `_CompassPanelState` so it survives across
// rebuilds. Dispose with the state.

import 'dart:ui' as ui;
import 'dart:math' as math;
import 'package:flutter/material.dart';
import '../../dxspot/projection.dart';

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
  ) =>
      'w=${cx.toStringAsFixed(1)},${cy.toStringAsFixed(1)}|'
      'r=${r.toStringAsFixed(1)}|'
      'z=${zoom.toStringAsFixed(2)}|'
      'c=${(centerLat ?? 0).toStringAsFixed(2)},${(centerLng ?? 0).toStringAsFixed(2)}|'
      'n=$ringCount';

  /// Raster the world coastline layer to the cache (when the key changes) and
  /// blit it onto [canvas] covering the whole widget rect. Returns `true` if
  /// the cache was rebuilt this call. No-op (returns false) when there's no
  /// center or rings to render.
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
    Color strokeColor,
  ) {
    if (centerLat == null || centerLng == null || rings == null || rings.isEmpty) {
      // Without data, drop any stale cache.
      _key = null;
      return false;
    }
    final key = _makeKey(cx, cy, r, zoom, centerLat, centerLng, rings.length);
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
    final stroke = Paint()
      ..color = strokeColor
      ..style = PaintingStyle.stroke
      ..strokeWidth = 0.8
      ..strokeJoin = StrokeJoin.round
      ..strokeCap = StrokeCap.round;
    final aeqd = Aeqd(centerLat, centerLng);
    final scale = (r - 6) * zoom / math.pi;
    for (final ring in rings) {
      Offset? prev;
      for (final p in ring) {
        final n = aeqd.normalized(p.lat, p.lng);
        if (n == null) {
          prev = null;
          continue;
        }
        final px = cx + n.x * scale;
        final py = cy - n.y * scale;
        if (prev != null) {
          c.drawLine(prev, Offset(px, py), stroke);
        }
        prev = Offset(px, py);
      }
    }
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