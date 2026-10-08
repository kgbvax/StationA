// vhf_map_test.dart — the VHF/UHF map module (DxMapContainer.vhf): Mercator
// only, UHF az beam, tap-to-aim on the great-circle bearing, PARK and STOP
// for both sat axes, town-level zoom.

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';
import 'package:hf_console/dxspot/places.dart';
import 'package:hf_console/dxspot/projection.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/sat_track.dart';
import 'package:hf_console/ui/widgets/dx_map_container.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';

import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

const _muenster = (lat: 51.962, lng: 7.626);
const _size = Size(800, 600);

Future<(BusStore, FakeMqttService)> _pump(WidgetTester tester, {String locator = 'JO32WE', bool online = true}) async {
  final store = BusStore();
  if (online) {
    store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 90, target: 90);
    store.setSatRotator('muehle/uhf/el-rotator', axis: 'el', pos: 10);
  }
  final mqtt = FakeMqttService(store);
  final dx = DxSpotService()..configure(locator: locator);
  await tester.binding.setSurfaceSize(_size);
  await tester.pumpWidget(TestHarness(
    store: store,
    mqtt: mqtt,
    dxSpot: dx,
    child: const SizedBox.expand(child: DxMapContainer.vhf()),
  ));
  await tester.pump(const Duration(milliseconds: 100));
  return (store, mqtt);
}

/// Screen position of a lat/lng on the map at the opening zoom, centred on
/// the QTH (the map fills the test surface).
Offset _screenOf(String locator, LatLng p) {
  final qth = locatorToLatLng(locator);
  final proj = MercatorProjection(
      centerLat: qth.lat, centerLng: qth.lng, zoom: kVhfMapZoom, width: _size.width, height: _size.height);
  final m = proj.project(p.lat, p.lng)!;
  return Offset(m.x, m.y);
}

void main() {
  testWidgets('Mercator only: no projection toggle, heading chip invites a tap', (tester) async {
    await _pump(tester);
    expect(find.byType(MercatorMapPanel), findsOneWidget);
    expect(find.byIcon(Icons.explore), findsNothing);
    expect(find.byIcon(Icons.map), findsNothing);
    expect(find.text('AZ 90° · TAP TO AIM'), findsOneWidget);
    expect(find.text('STOP'), findsOneWidget);
  });

  testWidgets('tapping Münster aims the UHF az rotator at the great-circle bearing', (tester) async {
    final (_, mqtt) = await _pump(tester);
    await tester.tapAt(_screenOf('JO32WE', _muenster));
    await tester.pump();

    final aims = mqtt.publishes.where((p) => p.topic == 'muehle/uhf/az-rotator/cmd').toList();
    expect(aims, hasLength(1));
    final payload = jsonDecode(aims.single.payload) as Map<String, dynamic>;
    expect(payload['action'], 'goto');
    final expected = initialBearing(locatorToLatLng('JO32WE'), _muenster);
    expect(double.parse(payload['value'] as String), closeTo(expected, 1.5));
    expect(aims.single.retain, isFalse);
  });

  testWidgets('no aiming while the az rotator is offline', (tester) async {
    final (_, mqtt) = await _pump(tester, online: false);
    await tester.tapAt(_screenOf('JO32WE', _muenster));
    await tester.pump();
    expect(mqtt.publishes, isEmpty);
    expect(find.text('AZ ROTATOR OFFLINE'), findsOneWidget);
  });

  testWidgets('STOP stops both sat axes, unretained', (tester) async {
    final (_, mqtt) = await _pump(tester);
    await tester.tap(find.text('STOP'));
    await tester.pump();
    final topics = mqtt.publishes.map((p) => p.topic).toSet();
    expect(topics, {'muehle/uhf/az-rotator/cmd', 'muehle/uhf/el-rotator/cmd'});
    for (final p in mqtt.publishes) {
      expect(jsonDecode(p.payload)['action'], 'stop');
      expect(p.retain, isFalse);
    }
  });

  testWidgets('PARK sends one bridge park intent to the az slot, unretained', (tester) async {
    final (_, mqtt) = await _pump(tester);
    await tester.tap(find.text('PARK'));
    await tester.pump();
    expect(mqtt.publishes.map((p) => p.topic), ['muehle/uhf/az-rotator/cmd']);
    expect(jsonDecode(mqtt.publishes.single.payload), {'action': 'park'});
    expect(mqtt.publishes.single.retain, isFalse);
  });

  testWidgets('PARK is disabled while both sat axes are offline', (tester) async {
    final (_, mqtt) = await _pump(tester, online: false);
    await tester.tap(find.text('PARK'));
    await tester.pump();
    expect(mqtt.publishes, isEmpty);
  });

  testWidgets('zooms in to 12 and no further', (tester) async {
    await _pump(tester);
    for (var i = 0; i < 20; i++) {
      await tester.tap(find.byIcon(Icons.add));
      await tester.pump();
    }
    // 10 × 0.5 steps from 7 reach 12; the + control then disables (muted
    // colour) while − stays live.
    expect(tester.widget<Icon>(find.byIcon(Icons.add)).color, AppTheme.txtMute);
    expect(tester.widget<Icon>(find.byIcon(Icons.remove)).color, AppTheme.txt);
  });

  test('bundled place parser: rows, bad rows skipped, garbage empty', () {
    final places = Places.parse('[["Münster",51.9704,7.62,7],["bad"],[1,2,3,4]]');
    expect(places, hasLength(1));
    expect(places.single.name, 'Münster');
    expect(places.single.rank, 7);
    expect(Places.parse('not json'), isEmpty);
    expect(Places.maxRankForZoom(kVhfMapZoom), greaterThanOrEqualTo(7), reason: 'Münster is visible at the opening zoom');
  });

  testWidgets('unrelated bus traffic does not repaint the map; a rotator move does', (tester) async {
    final (store, _) = await _pump(tester);
    await tester.pump();
    MercatorPainterDebug.paintCount = 0;
    for (var i = 0; i < 20; i++) {
      store.setPaTransmitting(fwd: 100.0 + i); // PA telemetry: many messages a second in the shack
      await tester.pump();
    }
    expect(MercatorPainterDebug.paintCount, 0);

    store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 120, target: 120);
    await tester.pump();
    expect(MercatorPainterDebug.paintCount, greaterThan(0));
  });

  group('tracked satellite', () {
    void track(BusStore store, {double lat = 47.0, double lng = 12.0, String name = 'ISS'}) {
      store.applyStatus(satTrackSlot, 'online');
      store.applyState(satTrackSlot, {
        'ts': '2026-10-07T11:05:00Z',
        'device_online': true,
        'tracking': true,
        'in_range': true,
        'sat_name': name,
        'el': 23.8,
        'range_km': 1012.4,
        'sub_lat': lat,
        'sub_lng': lng,
        'alt_km': 418.0,
      });
    }

    // The view the painter last drew, and where a point lands on screen.
    Offset? screenOf(double lat, double lng) {
      final p = MercatorPainterDebug.lastProjection!.project(lat, lng);
      return p == null ? null : Offset(p.x, p.y);
    }

    testWidgets('auto-fit frames the station and the satellite together', (tester) async {
      final (store, _) = await _pump(tester);
      final before = MercatorPainterDebug.lastProjection!.zoom;
      expect(before, kVhfMapZoom);

      track(store, lat: 40.0, lng: -25.0); // ~3500 km away
      await tester.pump();

      final proj = MercatorPainterDebug.lastProjection!;
      expect(proj.zoom, lessThan(before), reason: 'zoomed out to take in the satellite');
      for (final pt in [(51.962, 7.626), (40.0, -25.0)]) {
        final o = screenOf(pt.$1, pt.$2)!;
        expect(o.dx, inInclusiveRange(0, _size.width));
        expect(o.dy, inInclusiveRange(0, _size.height));
      }

      // The satellite sets: back to the station view.
      store.applyState(satTrackSlot, {'device_online': true, 'tracking': false});
      await tester.pump();
      expect(MercatorPainterDebug.lastProjection!.zoom, kVhfMapZoom);
    });

    testWidgets('a pinch takes the view over from auto-fit; RESET re-frames', (tester) async {
      final (store, _) = await _pump(tester);
      track(store, lat: 40.0, lng: -25.0);
      await tester.pump();
      final fitted = MercatorPainterDebug.lastProjection!.zoom;

      // Spread two fingers apart: zooms in about their centre.
      const c = Offset(400, 300);
      final g1 = await tester.startGesture(c - const Offset(20, 0), pointer: 1);
      final g2 = await tester.startGesture(c + const Offset(20, 0), pointer: 2);
      for (var i = 1; i <= 10; i++) {
        await g1.moveTo(c - Offset(20.0 + i * 12, 0));
        await g2.moveTo(c + Offset(20.0 + i * 12, 0));
        await tester.pump();
      }
      await g1.up();
      await g2.up();
      await tester.pump();
      final pinched = MercatorPainterDebug.lastProjection!.zoom;
      expect(pinched, greaterThan(fitted + 0.5));

      // A satellite move no longer re-frames the operator's view.
      track(store, lat: 41.0, lng: -24.0);
      await tester.pump();
      expect(MercatorPainterDebug.lastProjection!.zoom, pinched);

      // RESET (the crosshair) hands the view back to auto-fit.
      await tester.tap(find.byIcon(Icons.my_location));
      await tester.pump();
      expect(MercatorPainterDebug.lastProjection!.zoom, fitted);
    });

    testWidgets('pinch-out zooms out and stays inside the zoom limits', (tester) async {
      final (_, _) = await _pump(tester);
      const c = Offset(400, 300);
      final g1 = await tester.startGesture(c - const Offset(150, 0), pointer: 1);
      final g2 = await tester.startGesture(c + const Offset(150, 0), pointer: 2);
      for (var i = 1; i <= 30; i++) {
        await g1.moveTo(c - Offset(150.0 - i * 5, 0));
        await g2.moveTo(c + Offset(150.0 - i * 5, 0));
        await tester.pump();
      }
      await g1.up();
      await g2.up();
      await tester.pump();
      final z = MercatorPainterDebug.lastProjection!.zoom;
      expect(z, lessThan(kVhfMapZoom));
      expect(z, greaterThanOrEqualTo(1.0));
    });

    testWidgets('the beam is accent without a satellite and amber while tracking', (tester) async {
      final (store, _) = await _pump(tester);
      expect(MercatorPainterDebug.lastBeam!.color, AppTheme.accent);
      track(store);
      await tester.pump();
      expect(MercatorPainterDebug.lastBeam!.color, AppTheme.amber);
    });

    testWidgets('a satellite move repaints the map', (tester) async {
      final (store, _) = await _pump(tester);
      track(store);
      await tester.pump();
      expect(tester.takeException(), isNull);

      MercatorPainterDebug.paintCount = 0;
      track(store, lng: 13.5);
      await tester.pump();
      expect(MercatorPainterDebug.paintCount, greaterThan(0));
    });

    testWidgets('an identical republish does not repaint', (tester) async {
      final (store, _) = await _pump(tester);
      track(store);
      await tester.pump();
      MercatorPainterDebug.paintCount = 0;
      track(store);
      await tester.pump();
      expect(MercatorPainterDebug.paintCount, 0);
    });

    testWidgets('a footprint across the map seam paints without throwing', (tester) async {
      final (store, _) = await _pump(tester);
      // Centre ~8° E → the wrap seam sits at ~-172°: a bird over the Pacific
      // puts its footprint across it.
      track(store, lat: 10.0, lng: -170.0, name: 'AO-91');
      await tester.pump();
      expect(tester.takeException(), isNull);
    });
  });
}
