import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/band_legend.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';

import '../../support/test_harness.dart';

// The band key shared by both map projections. On the UHF page the feed is
// narrowed server-side to 2m/70cm — the key is what makes that contract
// visible, so its ordering and presence get pinned here.

DxSpotService _serviceWithBands(List<String> bands) {
  final dx = DxSpotService();
  dx.configure(locator: 'JO31');
  for (var i = 0; i < bands.length; i++) {
    dx.ingest(jsonEncode({
      'lat': 51.0 + i * 0.1,
      'lng': 7.0 + i * 0.1,
      'snr': 10,
      'ageSeconds': 0,
      'locator': 'JO3$i',
      'band': bands[i],
      'sourceType': 'mqtt',
    }));
  }
  return dx;
}

void main() {
  test('visibleBands orders HF bands first, then 2m and 70cm', () {
    final dx = _serviceWithBands(['70cm', '20m', '2m', '40m', '']);
    // The unbanded spot ('') is excluded; the rest follow kBandOrder.
    expect(visibleBands(dx.spots), ['40m', '20m', '2m', '70cm']);
  });

  testWidgets('BandLegend renders one chip per band', (tester) async {
    await tester.pumpWidget(MaterialApp(
      home: Scaffold(
        body: const BandLegend(visible: ['2m', '70cm'], vertical: true),
      ),
    ));
    expect(find.text('2m'), findsOneWidget);
    expect(find.text('70cm'), findsOneWidget);
  });

  testWidgets('Mercator map shows the band key for the bands in view', (tester) async {
    final dx = _serviceWithBands(['2m', '70cm']);
    await tester.pumpWidget(TestHarness(
      store: BusStore(),
      dxSpot: dx,
      child: const MercatorMapPanel(),
    ));
    await tester.pump(const Duration(milliseconds: 600));

    expect(find.text('2m'), findsOneWidget);
    expect(find.text('70cm'), findsOneWidget);
  });
}
