// vhf_map_test.dart — the VHF/UHF map module (DxMapContainer.vhf): Mercator
// only, UHF az beam, tap-to-aim on the great-circle bearing, E-STOP for both
// sat axes, town-level zoom.

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';
import 'package:hf_console/dxspot/places.dart';
import 'package:hf_console/dxspot/projection.dart';
import 'package:hf_console/store/bus_store.dart';
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
    expect(find.text('E-STOP'), findsOneWidget);
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

  testWidgets('E-STOP stops both sat axes, unretained', (tester) async {
    final (_, mqtt) = await _pump(tester);
    await tester.tap(find.text('E-STOP'));
    await tester.pump();
    final topics = mqtt.publishes.map((p) => p.topic).toSet();
    expect(topics, {'muehle/uhf/az-rotator/cmd', 'muehle/uhf/el-rotator/cmd'});
    for (final p in mqtt.publishes) {
      expect(jsonDecode(p.payload)['action'], 'stop');
      expect(p.retain, isFalse);
    }
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
}
