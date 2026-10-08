// overlay_render_test.dart — renders the Mercator beam / satellite overlay at
// a grid of zooms and map centres to PNGs under $OVERLAY_OUT (default
// /tmp/overlay) for eyeballing. Skipped unless OVERLAY_RENDER=1.

import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';
import 'package:hf_console/dxspot/projection.dart';
import 'package:hf_console/dxspot/world_geometry.dart';
import 'package:hf_console/store/sat_track.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';

const _size = Size(900, 600);

Future<void> _render(
  WidgetTester tester,
  String name, {
  required LatLng center,
  required double zoom,
  required LatLng qth,
  required double az,
  double half = 20,
  SatTrack? sat,
  List<List<LatLng>>? rings,
}) async {
  final proj = MercatorProjection(
      centerLat: center.lat, centerLng: center.lng, zoom: zoom, width: _size.width, height: _size.height);
  final painter = MercatorPainterDebug.overlayPainter(
    projection: proj,
    qth: qth,
    rings: rings,
    beam: (qth: qth, az: az, target: az + 40, half: half, online: true, color: const Color(0xFF4DD0E1)),
    sat: sat,
  );
  await tester.runAsync(() async {
    final rec = ui.PictureRecorder();
    painter.paint(Canvas(rec), _size);
    final img = await rec.endRecording().toImage(_size.width.toInt(), _size.height.toInt());
    final bytes = await img.toByteData(format: ui.ImageByteFormat.png);
    final dir = Directory(Platform.environment['OVERLAY_OUT'] ?? '/tmp/overlay')..createSync(recursive: true);
    File('${dir.path}/$name.png').writeAsBytesSync(bytes!.buffer.asUint8List());
  });
}

void main() {
  final enabled = Platform.environment['OVERLAY_RENDER'] == '1';
  testWidgets('render overlay grid', skip: !enabled, (tester) async {
    List<List<LatLng>>? rings;
    await tester.runAsync(() async => rings = await WorldGeometry.instance.load());
    const muenster = (lat: 51.962, lng: 7.626);
    const sats = <String, SatTrack>{
      'satnear': SatTrack(name: 'AO-91', lat: 40, lng: -20, altKm: 800, az: 280, el: 5, rangeKm: 3000, inRange: true),
      'satpole': SatTrack(name: 'NOAA', lat: 87, lng: 120, altKm: 850, az: 5, el: 3, rangeKm: 4000, inRange: true),
      'satsouth': SatTrack(name: 'SO-50', lat: -60, lng: -170, altKm: 650, az: 200, el: 0, rangeKm: 9000),
    };
    for (final zoom in [1.0, 2.0, 3.0, 5.0, 8.0]) {
      for (final c in <String, LatLng>{
        'home': muenster,
        'north': (lat: 80.0, lng: 8.0),
        'south': (lat: -80.0, lng: 8.0),
      }.entries) {
        for (final az in [0.0, 90.0, 180.0, 290.0]) {
          await _render(tester, 'beam_z${zoom.toInt()}_${c.key}_az${az.toInt()}',
              center: c.value, zoom: zoom, qth: muenster, az: az, rings: rings);
        }
        for (final s in sats.entries) {
          await _render(tester, 'sat_z${zoom.toInt()}_${c.key}_${s.key}',
              center: c.value, zoom: zoom, qth: muenster, az: 45, sat: s.value, rings: rings);
        }
      }
    }
    // QTH near a pole.
    for (final q in <String, LatLng>{'qthN': (lat: 78.2, lng: 15.6), 'qthS': (lat: -77.8, lng: 166.7)}.entries) {
      for (final zoom in [1.0, 2.0, 4.0]) {
        for (final az in [0.0, 90.0, 180.0, 270.0]) {
          await _render(tester, 'beam_${q.key}_z${zoom.toInt()}_az${az.toInt()}',
              center: q.value, zoom: zoom, qth: q.value, az: az, rings: rings);
        }
      }
    }
  });
}
