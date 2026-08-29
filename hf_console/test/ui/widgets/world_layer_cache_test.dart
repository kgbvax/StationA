// world_layer_cache_test.dart — verifies the cache key/hit behavior of the
// world landmass raster cache that the compass panel uses to keep zoom-drag
// frames fast.
//
// The cache is exercised end-to-end through its public `draw()` method with a
// dummy Canvas. We assert the rasterization is skipped (returned `false`) on
// a second call with the same key, and fires again (returned `true`) when any
// cache-key input changes. Real pixel-content checks would require a golden
// file; key/hit behavior is the part that gates 60fps vs 1fps.

import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/ui/widgets/world_layer_cache.dart';
import 'package:hf_console/dxspot/projection.dart';

void main() {
  // `paintImage` and `toImageSync` both reach into PaintingBinding for an
  // ImageCache. Initialize the binding once for the whole file.
  TestWidgetsFlutterBinding.ensureInitialized();

  // Minimal FeatureCollection — one tiny ring around (0,0). The cache only
  // cares that the list is non-empty; geometry doesn't influence hit/miss.
  final rings = <List<LatLng>>[
    [
      (lat: 0.0, lng: 0.0),
      (lat: 1.0, lng: 0.0),
      (lat: 1.0, lng: 1.0),
      (lat: 0.0, lng: 1.0),
      (lat: 0.0, lng: 0.0),
    ],
  ];

  Canvas dummyCanvas() => Canvas(ui.PictureRecorder());

  test('first call rebuilds (returns true), second call with same key hits cache (returns false)', () {
    final cache = WorldLayerCache();
    final canvas = dummyCanvas();
    final rect = Offset.zero & const Size(400, 400);
    bool rebuilt = cache.draw(
      canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt, isTrue, reason: 'first call must rebuild the raster');
    rebuilt = cache.draw(
      canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt, isFalse, reason: 'second call with identical inputs must hit the cache');
    cache.dispose();
  });

  test('changing zoom invalidates the cache', () {
    final cache = WorldLayerCache();
    final canvas = dummyCanvas();
    final rect = Offset.zero & const Size(400, 400);
    cache.draw(canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF));
    final rebuilt = cache.draw(
      canvas, rect, 200, 200, 100, 2.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt, isTrue);
    cache.dispose();
  });

  test('changing the AEQD center invalidates the cache', () {
    final cache = WorldLayerCache();
    final canvas = dummyCanvas();
    final rect = Offset.zero & const Size(400, 400);
    cache.draw(canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF));
    // Center shifts from (50, 8) → (52, 9). The 0.01° rounding in the cache
    // key means a sub-0.01° shift is a cache hit; 2° is a definite miss.
    final rebuilt = cache.draw(
      canvas, rect, 200, 200, 100, 1.5, 52.0, 9.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt, isTrue);
    cache.dispose();
  });

  test('changing the fill color (theme switch) invalidates the cache', () {
    final cache = WorldLayerCache();
    final canvas = dummyCanvas();
    final rect = Offset.zero & const Size(400, 400);
    cache.draw(canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0,
        rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF));
    final rebuilt = cache.draw(canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0,
        rings, const Color(0xFFDFDAD0), const Color(0xFFFFFFFF));
    expect(rebuilt, isTrue, reason: 'a theme switch must re-rasterize the land fill');
    cache.dispose();
  });

  test('null center is a no-op and clears any previous cache', () {
    final cache = WorldLayerCache();
    final canvas = dummyCanvas();
    final rect = Offset.zero & const Size(400, 400);
    cache.draw(canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF));
    final rebuilt = cache.draw(
      canvas, rect, 200, 200, 100, 1.5, null, null, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt, isFalse);
    final rebuilt2 = cache.draw(
      canvas, rect, 200, 200, 100, 1.5, 50.0, 8.0, rings, const Color(0xFF232C3E), const Color(0xFFFFFFFF),
    );
    expect(rebuilt2, isTrue);
    cache.dispose();
  });
}