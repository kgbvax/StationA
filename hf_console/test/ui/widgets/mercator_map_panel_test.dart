import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';

import '../../support/test_harness.dart';

void main() {
  testWidgets('MercatorMapPanel builds and switches when no QTH is set', (tester) async {
    await tester.pumpWidget(
      const TestHarness(child: MercatorMapPanel()),
    );
    expect(find.byType(MercatorMapPanel), findsOneWidget);
  });

  testWidgets('MercatorMapPanel paints spots configured from DxSpotService', (tester) async {
    final dx = DxSpotService();
    dx.configure(locator: 'JO31');
    dx.ingest(jsonEncode({
      'lat': 51.0,
      'lng': 7.0,
      'snr': 5,
      'ageSeconds': 0,
      'locator': 'JO31',
      'band': '20m',
      'sourceType': 'mqtt',
    }));

    await tester.pumpWidget(
      TestHarness(dxSpot: dx, child: const MercatorMapPanel()),
    );
    // Drain the DxSpotService notify throttle (500 ms) so no timer leaks.
    await tester.pump(const Duration(milliseconds: 600));

    expect(find.byType(MercatorMapPanel), findsOneWidget);
    expect(dx.spots.length, 1);
  });

  test('grid squares fade as the map zooms in', () {
    expect(gridZoomFade(2.5), 1.0);
    expect(gridZoomFade(4), 1.0);
    expect(gridZoomFade(6), closeTo(0.65, 1e-9));
    expect(gridZoomFade(7), closeTo(0.475, 1e-9));
    expect(gridZoomFade(8), 0.3);
    expect(gridZoomFade(12), 0.3);
  });
}
