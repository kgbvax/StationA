import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/selected_spot.dart';

void main() {
  final fullState = <String, dynamic>{
    'ts': '2026-09-15T14:03:12Z',
    'device_online': true,
    'selected': <String, dynamic>{
      'call': 'vk9xy',
      'freq_hz': 14024760,
      'band': '20m',
      'mode': 'cw',
      'lat': -11.53,
      'lng': 151.19,
      'azimuth': 62.4,
      'distance_km': 15420.3,
      'country_prefix': 'VK9',
      'source': 'dxlog',
      'ts': '2026-09-15T14:03:12Z',
    },
  };

  test('parses the full selected record', () {
    final sel = SelectedSpot.fromSelected(fullState['selected']);
    expect(sel, isNotNull);
    expect(sel!.call, 'VK9XY'); // uppercased
    expect(sel.freqHz, 14024760);
    expect(sel.band, '20m');
    expect(sel.mode, 'cw');
    expect(sel.lat, -11.53);
    expect(sel.lng, 151.19);
    expect(sel.azimuth, 62.4);
    expect(sel.distanceKm, 15420.3);
    expect(sel.countryPrefix, 'VK9');
    expect(sel.source, 'dxlog');
    expect(sel.hasPosition, isTrue);
    expect(sel.hasBearing, isTrue);
  });

  test('azimuth-only record has a bearing but no position', () {
    final sel = SelectedSpot.fromSelected({
      'call': 'AB1CDE',
      'azimuth': 270.0,
      'source': 'dxlog',
      'ts': '2026-09-15T14:03:12Z',
    });
    expect(sel, isNotNull);
    expect(sel!.hasBearing, isTrue);
    expect(sel.hasPosition, isFalse);
  });

  test('null / non-map / empty call all yield null (the clear signal)', () {
    expect(SelectedSpot.fromSelected(null), isNull);
    expect(SelectedSpot.fromSelected('garbage'), isNull);
    expect(SelectedSpot.fromSelected(<String, dynamic>{}), isNull);
    expect(SelectedSpot.fromSelected(<String, dynamic>{'call': ''}), isNull);
    expect(SelectedSpot.fromSelected(<String, dynamic>{'call': '  '}), isNull);
  });

  test('unparseable ts ages the record out immediately', () {
    final sel = SelectedSpot.fromSelected({'call': 'AB1CDE'});
    expect(sel, isNotNull);
    expect(
      stalenessFor(sel!.ageSecondsAt(DateTime.now().millisecondsSinceEpoch)),
      SelectedStaleness.expired,
    );
  });

  test('age buckets: fresh, stale, expired', () {
    final now = DateTime.parse('2026-09-15T15:00:00Z').millisecondsSinceEpoch;
    int ageFor(String ts) {
      final sel = SelectedSpot.fromSelected({'call': 'AB1CDE', 'ts': ts});
      return sel!.ageSecondsAt(now);
    }

    expect(stalenessFor(ageFor('2026-09-15T14:59:30Z')), SelectedStaleness.fresh);
    expect(stalenessFor(ageFor('2026-09-15T14:54:00Z')), SelectedStaleness.stale);
    expect(stalenessFor(ageFor('2026-09-15T14:30:00Z')), SelectedStaleness.expired);
  });

  test('selectedChipVisible: shown fresh or stale, gone once expired', () {
    final now = DateTime.parse('2026-09-15T15:00:00Z').millisecondsSinceEpoch;
    Map<String, dynamic> sel(String ts) => {'call': 'AB1CDE', 'ts': ts};

    expect(selectedChipVisible(sel('2026-09-15T14:59:30Z'), now), isTrue); // fresh
    expect(selectedChipVisible(sel('2026-09-15T14:54:00Z'), now), isTrue); // stale — dimmed, not gone
    expect(selectedChipVisible(sel('2026-09-15T14:30:00Z'), now), isFalse); // expired
    expect(selectedChipVisible(null, now), isFalse); // nothing keyed
    expect(selectedChipVisible(<String, dynamic>{}, now), isFalse); // empty call
  });
}
