import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/dxspot/dxspot_source.dart';

void main() {
  group('DxSpotFilter', () {
    test('defaults to SSB mode with 0 dB threshold', () {
      const f = DxSpotFilter();
      expect(f.mode, 'ssb');
      expect(f.ssbMinDb, 0);
      expect(f.cwMinDb, -15);
      expect(f.threshold, 0);
    });

    test('maps radio modes to SNR families', () {
      expect(DxSpotFilter.snrModeFor('USB'), 'ssb');
      expect(DxSpotFilter.snrModeFor('lsb'), 'ssb');
      expect(DxSpotFilter.snrModeFor('am'), 'ssb');
      expect(DxSpotFilter.snrModeFor('fm'), 'ssb');
      expect(DxSpotFilter.snrModeFor('data'), 'ssb');
      expect(DxSpotFilter.snrModeFor('CW'), 'cw');
      expect(DxSpotFilter.snrModeFor(null), 'none');
      expect(DxSpotFilter.snrModeFor(''), 'none');
      expect(DxSpotFilter.snrModeFor('FT8'), 'none');
    });

    test('allows any SNR in none mode', () {
      const f = DxSpotFilter(mode: 'none');
      expect(f.allows(-30), true);
      expect(f.allows(30), true);
    });

    test('SSB threshold defaults to 0 dB', () {
      const f = DxSpotFilter(mode: 'ssb');
      expect(f.allows(-1), false);
      expect(f.allows(0), true);
      expect(f.allows(5), true);
    });

    test('CW threshold defaults to -15 dB', () {
      const f = DxSpotFilter(mode: 'cw');
      expect(f.allows(-16), false);
      expect(f.allows(-15), true);
      expect(f.allows(-10), true);
    });
  });

  group('streamUrl', () {
    test('uses default base URL when empty', () {
      final url = DxSpotService.streamUrl('', 'JO31');
      expect(url, startsWith('https://horstreporter.kgbvax.net/api/stream'));
    });

    test('encodes QTH and requests 30 min + surroundings', () {
      final url = DxSpotService.streamUrl('https://example.com', 'JO31OM');
      expect(url, 'https://example.com/api/stream?qth=JO31OM&minutes=30&surroundings=true');
    });
  });

  group('ingest filtering', () {
    late DxSpotService service;

    setUp(() {
      service = DxSpotService();
      service.configure(locator: 'JO31');
    });

    String spotJson({
      required String sourceType,
      required int snr,
      required String locator,
      String band = '20m',
    }) {
      return jsonEncode({
        'lat': 51.0,
        'lng': 7.0,
        'snr': snr,
        'ageSeconds': 0,
        'locator': locator,
        'band': band,
        'sourceType': sourceType,
      });
    }

    test('keeps mqtt spots, drops other source types', () {
      service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'dxcluster', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'rbn', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'wspr', snr: 5, locator: 'JO31'));
      expect(service.spots.length, 1);
      expect(service.spots.first.sourceType, 'mqtt');
    });

    test('drops mqtt spots below the SSB threshold', () {
      service.setFilter(const DxSpotFilter(mode: 'ssb', ssbMinDb: 0));
      service.ingest(spotJson(sourceType: 'mqtt', snr: -2, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'mqtt', snr: 0, locator: 'JO32'));
      service.ingest(spotJson(sourceType: 'mqtt', snr: 3, locator: 'JO33'));
      expect(service.spots.length, 2);
      expect(service.spots.map((s) => s.locator).toSet(), {'JO32', 'JO33'});
    });

    test('drops mqtt spots below the CW threshold', () {
      service.setFilter(const DxSpotFilter(mode: 'cw', cwMinDb: -15));
      service.ingest(spotJson(sourceType: 'mqtt', snr: -16, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'mqtt', snr: -15, locator: 'JO32'));
      expect(service.spots.length, 1);
      expect(service.spots.first.snr, -15);
    });

    test('setMode updates the filter from a radio mode', () {
      service.setMode('USB');
      expect(service.filter.mode, 'ssb');
      service.setMode('cw');
      expect(service.filter.mode, 'cw');
      service.setMode('');
      expect(service.filter.mode, 'none');
    });
  });

  group('topQuartileMean', () {
    test('matches horstreporter for typical SNR lists', () {
      // One spot -> the spot itself.
      expect(DxSpotService.topQuartileMean([5]), closeTo(5.0, 0.0001));
      // Four spots -> top 1 (k=ceil(4/4)=1).
      expect(DxSpotService.topQuartileMean([1, 2, 3, 4]), closeTo(4.0, 0.0001));
      // Eight spots -> top 2.
      expect(DxSpotService.topQuartileMean([1, 2, 3, 4, 5, 6, 7, 8]), closeTo(7.5, 0.0001));
      // Unordered input.
      expect(DxSpotService.topQuartileMean([8, 2, 7, 1, 6, 3, 5, 4]), closeTo(7.5, 0.0001));
    });

    test('empty list returns NaN', () {
      expect(DxSpotService.topQuartileMean([]).isNaN, true);
    });
  });

  group('grid-square aggregation', () {
    test('dominant band and score use all spots in the square', () {
      final service = DxSpotService();
      service.configure(locator: 'JO31');
      service.ingest(jsonEncode({
        'lat': 51.0,
        'lng': 7.0,
        'snr': 10,
        'ageSeconds': 0,
        'locator': 'JO31',
        'band': '20m',
        'sourceType': 'mqtt',
      }));
      service.ingest(jsonEncode({
        'lat': 51.1,
        'lng': 7.1,
        'snr': 6,
        'ageSeconds': 0,
        'locator': 'JO31',
        'band': '20m',
        'sourceType': 'mqtt',
      }));
      expect(service.gridSquares.length, 1);
      final sq = service.gridSquares.first;
      expect(sq.locator, 'JO31');
      expect(sq.dominantBand, '20m');
      // Two SNRs -> k = ceil(2/4) = 1 -> max SNR.
      expect(sq.score, closeTo(10.0, 0.0001));
    });
  });

  group('setBands (server-side subscription narrowing)', () {
    late DxSpotService service;
    late List<String> startedUrls;

    setUp(() {
      service = DxSpotService();
      service.configure(locator: 'JO31');
      startedUrls = [];
      DxSpotService.debugSourceFactory = () => _RecordingSource(onStart: startedUrls.add);
    });

    tearDown(() {
      DxSpotService.debugSourceFactory = null;
      service.stop();
    });

    test('streamUrl carries enabled_bands when set, omits it otherwise', () {
      expect(
        DxSpotService.streamUrl('https://example.com', 'JO31OM', bands: const {'2m', '70cm'}),
        'https://example.com/api/stream?qth=JO31OM&minutes=30&surroundings=true&enabled_bands=2m,70cm',
      );
      expect(
        DxSpotService.streamUrl('https://example.com', 'JO31OM'),
        'https://example.com/api/stream?qth=JO31OM&minutes=30&surroundings=true',
      );
    });

    test('setBands re-dials with the narrowed URL; unchanged set does not', () {
      service.start();
      expect(startedUrls, hasLength(1));
      expect(startedUrls.single, isNot(contains('enabled_bands')));

      service.setBands(const {'2m', '70cm'});
      expect(startedUrls, hasLength(2));
      expect(startedUrls.last, contains('enabled_bands=2m,70cm'));

      // Same set in different order: no reconnect.
      service.setBands({'70cm', '2m'});
      expect(startedUrls, hasLength(2));

      // Back to unrestricted: re-dial without the parameter.
      service.setBands(null);
      expect(startedUrls, hasLength(3));
      expect(startedUrls.last, isNot(contains('enabled_bands')));
    });

    test('setBands purges spots from the previous band set immediately', () {
      service.start();
      service.ingest(jsonEncode({
        'lat': 51.0,
        'lng': 7.0,
        'snr': 10,
        'ageSeconds': 0,
        'locator': 'JO31',
        'band': '40m',
        'sourceType': 'mqtt',
      }));
      expect(service.spots, isNotEmpty);

      // Without this purge the 40m dot would linger up to the 10-minute
      // age-out on the narrowed feed — the replay only adds, never removes.
      service.setBands(const {'2m', '70cm'});
      expect(service.spots, isEmpty);
      expect(service.gridSquares, isEmpty);
    });
  });
}

/// Records every start URL; never connects, never reports disconnection.
class _RecordingSource implements DxSpotSource {
  _RecordingSource({required this.onStart});

  final void Function(String url) onStart;

  @override
  void start(
    String url, {
    required void Function(String data) onData,
    required void Function() onDisconnected,
  }) {
    onStart(url);
  }

  @override
  void stop() {}
}
