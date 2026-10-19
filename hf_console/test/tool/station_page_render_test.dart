// station_page_render_test.dart — renders the whole tablet console on the
// Station page (top bar, schematic, power card, faults) per scenario to PNGs
// under $PAGE_OUT (default /tmp/station-page), bundled fonts. Skipped unless
// PAGE_RENDER=1.

import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../support/fake_mqtt_service.dart';
import '../support/station_fixtures.dart';
import '../support/test_harness.dart';

const _families = {
  'IBMPlexMono': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'IBMPlexSans': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'SairaCondensed': ['Regular', 'Medium', 'SemiBold', 'Bold'],
};

Future<void> _loadFonts() async {
  for (final MapEntry(key: family, value: weights) in _families.entries) {
    final loader = FontLoader(family);
    for (final w in weights) {
      final bytes = File('assets/fonts/$family-$w.ttf').readAsBytesSync();
      loader.addFont(Future.value(ByteData.view(Uint8List.fromList(bytes).buffer)));
    }
    await loader.load();
  }
}

void main() {
  final enabled = Platform.environment['PAGE_RENDER'] == '1';

  testWidgets('render the Station page', skip: !enabled, (tester) async {
    await tester.runAsync(_loadFonts);
    // The console reads its settings on start; give it an empty store.
    SharedPreferences.setMockInitialValues({});
    final dir = Directory(Platform.environment['PAGE_OUT'] ?? '/tmp/station-page')..createSync(recursive: true);
    // Lenovo Tab landscape, logical 1334 x 800 at dpr 1.5.
    tester.view.devicePixelRatio = 1.5;
    tester.view.physicalSize = const Size(2001, 1200);
    addTearDown(tester.view.reset);

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
      final boundary = key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
      await tester.runAsync(() async {
        final ui.Image img = await boundary.toImage(pixelRatio: 1);
        final png = await img.toByteData(format: ui.ImageByteFormat.png);
        File('${dir.path}/${scenario.name}.png').writeAsBytesSync(png!.buffer.asUint8List());
      });
    }
  });
}
