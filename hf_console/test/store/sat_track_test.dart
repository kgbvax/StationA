// sat_track_test.dart — parsing muehle/uhf/sat-track (oscarwatch-sattrack-bridge)
// into the map's satellite: tracked + device_online + sub-satellite point, or
// nothing.

import 'dart:math' as math;

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/sat_track.dart';

import '../support/fixtures.dart';

Map<String, dynamic> _tracking({Map<String, dynamic> over = const {}}) => {
      'ts': '2026-10-07T11:05:00Z',
      'device_online': true,
      'tracking': true,
      'in_range': true,
      'sat_name': 'ISS',
      'norad_id': '25544',
      'az': 201.3,
      'el': 23.8,
      'range_km': 1012.4,
      'sub_lat': 46.5,
      'sub_lng': 3.25,
      'alt_km': 418.0,
      ...over,
    };

void main() {
  test('a tracked satellite with a sub-satellite point parses', () {
    final s = SatTrack.fromState(_tracking())!;
    expect(s.name, 'ISS');
    expect(s.lat, 46.5);
    expect(s.lng, 3.25);
    expect(s.inRange, isTrue);
    expect(s.label, 'ISS  EL 24°  1012 km');
  });

  test('nothing to draw: not tracking, tracker down, or no sub-point', () {
    // The bridge publishes nulls (not omissions) when nothing is tracked.
    final idle = {
      'ts': '2026-10-07T11:20:00Z',
      'device_online': true,
      'tracking': false,
      'in_range': false,
      'sat_name': null,
      'sub_lat': null,
      'sub_lng': null,
    };
    expect(SatTrack.fromState(idle), isNull);
    expect(SatTrack.fromState(_tracking(over: {'device_online': false})), isNull);
    expect(SatTrack.fromState(_tracking(over: {'sub_lat': null, 'sub_lng': null})), isNull);
    expect(SatTrack.fromState(null), isNull);
  });

  test('footprint radius is the horizon distance for the altitude', () {
    final s = SatTrack.fromState(_tracking(over: {'alt_km': 800.0}))!;
    final expected = 6371.0 * math.acos(6371.0 / 7171.0); // ≈ 3037 km
    expect(s.footprintRadiusKm, closeTo(expected, 0.01));
    expect(SatTrack.fromState(_tracking(over: {'alt_km': null}))!.footprintRadiusKm, isNull);
  });

  test('a missing look angle shortens the label instead of failing', () {
    final s = SatTrack.fromState(_tracking(over: {'el': null, 'range_km': null, 'sat_name': null}))!;
    expect(s.label, 'SAT');
  });

  group('fromStore — two-layer liveness', () {
    test('needs the link, the bridge /status and device_online', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.applyState(satTrackSlot, _tracking());
      expect(SatTrack.fromStore(store), isNull, reason: 'no /status yet');

      store.applyStatus(satTrackSlot, 'online');
      expect(SatTrack.fromStore(store)?.name, 'ISS');

      store.applyStatus(satTrackSlot, 'offline');
      expect(SatTrack.fromStore(store), isNull, reason: 'bridge down: never a frozen position');
    });

    test('link down hides it', () {
      final store = BusStore();
      store.applyStatus(satTrackSlot, 'online');
      store.applyState(satTrackSlot, _tracking());
      expect(SatTrack.fromStore(store), isNull);
    });
  });
}
