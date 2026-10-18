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

    test('encodes QTH, requests the 10-min window, drops WSPR/DX-cluster server-side', () {
      final url = DxSpotService.streamUrl('https://example.com', 'JO31OM');
      expect(url,
          'https://example.com/api/stream?qth=JO31OM&minutes=10&include_wspr=false&include_dxcluster=false');
      expect(url, isNot(contains('surroundings')));
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

    test('a burst of spots is derived once per read, not once per spot', () {
      // The connect replays the 30-minute history as one event per spot.
      // Re-deriving (prune + sort + grid) on each froze Safari's JIT-less
      // Enhanced Security web process; the store may grow per event, the
      // derivation must wait for the reader.
      for (var i = 0; i < 2000; i++) {
        service.ingest(jsonEncode({
          'lat': 51.0,
          'lng': 7.0,
          'snr': i % 20,
          'ageSeconds': i % 600,
          'locator': 'JO${31 + i % 40}',
          'band': '20m',
          'sender': 'DL${i}AA',
          'sourceType': 'mqtt',
        }));
      }
      expect(service.rebuildCount, 0);
      expect(service.spots.length, 2000); // the whole window, not a freshest-N slice
      expect(service.gridSquares, isNotEmpty);
      expect(service.rebuildCount, 1); // spots + gridSquares share one pass
      service.spots;
      expect(service.rebuildCount, 1); // unchanged store: no re-derivation
    });

    test('keeps PSKReporter (mqtt) and RBN spots, drops other source types', () {
      service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'dxcluster', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'rbn', snr: 5, locator: 'JO31'));
      service.ingest(spotJson(sourceType: 'wspr', snr: 5, locator: 'JO31'));
      expect(service.spots.map((s) => s.sourceType).toSet(), {'mqtt', 'rbn'});
    });

    test('a burst of new squares does not evict the squares already in the window', () {
      // Regression: the overlay showed only the 80 freshest keys, so on a busy
      // feed every new spot pushed an older square off the map.
      for (var i = 0; i < 5; i++) {
        service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JN${10 + i}'));
      }
      for (var i = 0; i < 200; i++) {
        service.ingest(jsonEncode({
          'lat': 51.0, 'lng': 7.0, 'snr': 5, 'ageSeconds': 0,
          'locator': 'K${String.fromCharCode(65 + i % 18)}${(i ~/ 18) % 10}${i % 10}',
          'band': '20m', 'sender': 'X$i', 'sourceType': 'mqtt',
        }));
      }
      final locs = service.gridSquares.map((q) => q.locator).toSet();
      for (var i = 0; i < 5; i++) {
        expect(locs, contains('JN${10 + i}'));
      }
    });

    test('a replayed older report does not make a key look fresh', () {
      // Live report heard 300 s ago (as seen from the receipt instant).
      service.ingest(jsonEncode({
        'lat': 51.0, 'lng': 7.0, 'snr': 3, 'ageSeconds': 300,
        'locator': 'JO31', 'band': '20m', 'sourceType': 'mqtt',
      }));
      // Reconnect replay of an even older report of the same key.
      service.ingest(jsonEncode({
        'lat': 51.0, 'lng': 7.0, 'snr': 12, 'ageSeconds': 500,
        'locator': 'JO31', 'band': '20m', 'sourceType': 'mqtt',
      }));
      final s = service.spots.single;
      final now = DateTime.now().millisecondsSinceEpoch;
      expect(s.liveAgeSecondsAt(now), greaterThanOrEqualTo(300));
      expect(s.snr, 3); // the latest report's SNR, not the session max
      expect(s.reports, 2);
    });

    test('drops reports older than the age window at ingest', () {
      service.ingest(jsonEncode({
        'lat': 51.0, 'lng': 7.0, 'snr': 3, 'ageSeconds': 700,
        'locator': 'JO31', 'band': '20m', 'sourceType': 'mqtt',
      }));
      expect(service.spots, isEmpty);
    });

    test('an SNR-gate change re-filters the stored window at once', () {
      service.setFilter(const DxSpotFilter(mode: 'ssb', ssbMinDb: 0));
      service.ingest(spotJson(sourceType: 'mqtt', snr: -10, locator: 'JO31'));
      expect(service.spots, isEmpty);
      service.setMode('cw');
      expect(service.spots.single.snr, -10);
    });

    test('unchanged squares keep their list instance (no repaint)', () {
      service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31'));
      final first = service.gridSquares;
      service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31'));
      expect(identical(service.gridSquares, first), isTrue);
    });

    test('dominant band follows the report count, ties broken deterministically', () {
      for (final b in ['40m', '20m', '20m']) {
        service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31', band: b));
      }
      expect(service.gridSquares.single.dominantBand, '20m');
    });

    test('configure with a new QTH purges the previous spots', () {
      service.ingest(spotJson(sourceType: 'mqtt', snr: 5, locator: 'JO31'));
      expect(service.spots, isNotEmpty);
      service.configure(locator: 'JO40');
      expect(service.spots, isEmpty);
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
        'sender': 'DL1AA',
        'sourceType': 'mqtt',
      }));
      service.ingest(jsonEncode({
        'lat': 51.1,
        'lng': 7.1,
        'snr': 6,
        'ageSeconds': 0,
        'locator': 'JO31AB',
        'band': '20m',
        'sender': 'DL2BB',
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
        'https://example.com/api/stream?qth=JO31OM&minutes=10&include_wspr=false&include_dxcluster=false'
        '&enabled_bands=2m,70cm',
      );
      expect(
        DxSpotService.streamUrl('https://example.com', 'JO31OM'),
        'https://example.com/api/stream?qth=JO31OM&minutes=10&include_wspr=false&include_dxcluster=false',
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
