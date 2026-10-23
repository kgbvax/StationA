// station_page_render_test.dart — renders the whole tablet console on the
// Station page (top bar, schematic, power card, faults) per scenario to PNGs
// under $PAGE_OUT (default /tmp/station-page), bundled fonts. Skipped unless
// PAGE_RENDER=1.

import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../support/fake_mqtt_service.dart';
import '../support/render_support.dart';
import '../support/station_fixtures.dart';
import '../support/test_harness.dart';

void main() {
  final enabled = Platform.environment['PAGE_RENDER'] == '1';

  testWidgets('render the Station page', skip: !enabled, (tester) async {
    await tester.runAsync(loadRenderFonts);
    // The console reads its settings on start; give it an empty store.
    SharedPreferences.setMockInitialValues({});
    final dir = Directory(Platform.environment['PAGE_OUT'] ?? '/tmp/station-page')..createSync(recursive: true);
    setTabletView(tester);

    for (final scenario in [StationScenario.running, StationScenario.faults, StationScenario.linkDown]) {
      final store = BusStore()..seedScenario(scenario);
      final key = GlobalKey();
      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: FakeMqttService(store),
        child: RepaintBoundary(key: key, child: const ConsoleScreen()),
      ));
      await tester.pump(const Duration(milliseconds: 100));
      await tester.tap(find.widgetWithText(InkWell, 'Station'));
      await tester.pump(const Duration(milliseconds: 100));
      if (scenario == StationScenario.linkDown) {
        store.markDisconnected();
        await tester.pump();
      }
      await shootPng(tester, key, '${dir.path}/${scenario.name}.png');
      // The other pages once, from the running station.
      if (scenario == StationScenario.running) {
        for (final page in ['HF', 'UHF', 'CAM']) {
          await tester.tap(find.widgetWithText(InkWell, page));
          await tester.pump(const Duration(milliseconds: 100));
          await shootPng(tester, key, '${dir.path}/page-${page.toLowerCase()}.png');
        }
      }
    }
    // The HF page under a storm lock (the console opens on HF).
    final storm = BusStore()..seedScenario(StationScenario.storm);
    final key = GlobalKey();
    await tester.pumpWidget(TestHarness(
      store: storm,
      mqtt: FakeMqttService(storm),
      child: RepaintBoundary(key: key, child: const ConsoleScreen()),
    ));
    await tester.pump(const Duration(milliseconds: 100));
    await shootPng(tester, key, '${dir.path}/page-hf-storm.png');
  });
}
